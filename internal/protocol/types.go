// Package protocol defines immutable, authenticated v1 swap terms and safety gates.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/btcsuite/btcd/btcec/v2"
	"time"
)

const Confirmations = 2
const FundingFee int64 = 2000

var RescueFees = []int64{2000, 6000, 20000}

const LongBlocks uint32 = 96
const ShortBlocks uint32 = 48
const TakeoverBlocks uint32 = 32
const RevealBlocks uint32 = 24
const RefundGrace uint32 = 6

type Offer struct {
	ID          string   `json:"id"`
	Maker       string   `json:"maker"`
	Sell        chain.ID `json:"sell"`
	SellAmount  int64    `json:"sell_amount"`
	BuyAmount   int64    `json:"buy_amount"`
	TowerBPS    int64    `json:"tower_bps"`
	Expires     int64    `json:"expires"`
	Status      string   `json:"status"`
	Reservation string   `json:"reservation,omitempty"`
}

func (o Offer) Validate(now int64) error {
	if !Hex32(o.ID) || !Hex32(o.Maker) || !o.Sell.Valid() || o.SellAmount < 100000 || o.BuyAmount < 100000 || o.SellAmount > 10000000000 || o.BuyAmount > 10000000000 {
		return errors.New("invalid order bounds (regtest v1: 100,000 to 10 billion sats per leg)")
	}
	if o.TowerBPS < 0 || o.TowerBPS > 1000 {
		return errors.New("tower quote out of bounds")
	}
	if o.TowerBPS > 0 {
		for _, n := range []int64{o.SellAmount, o.BuyAmount} {
			fee := Bounty(n, o.TowerBPS)
			if fee < contract.Dust || n-fee-RescueFees[len(RescueFees)-1] < contract.Dust {
				return errors.New("trade below tower economic minimum")
			}
		}
	}
	if o.Expires <= now || o.Expires > now+7*24*3600 {
		return errors.New("order expired or too far in future")
	}
	if o.Status != "open" && o.Status != "reserved" && o.Status != "cancelled" && o.Status != "filled" {
		return errors.New("invalid order status")
	}
	return nil
}
func DecodeOffer(event nostr.Event, now int64) (Offer, error) {
	var o Offer
	if e := transport.Valid(event); e != nil {
		return o, e
	}
	if event.Kind != transport.OfferKind || transport.Tag(event, "t") != transport.Namespace {
		return o, errors.New("wrong order namespace")
	}
	if e := json.Unmarshal([]byte(event.Content), &o); e != nil {
		return o, e
	}
	if o.Maker != event.PubKey.Hex() || o.ID != transport.Tag(event, "d") {
		return o, errors.New("order signature binding mismatch")
	}
	if e := o.Validate(now); e != nil {
		return o, e
	}
	return o, nil
}
func Hex32(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func ValidKey(s string) bool {
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != 33 {
		return false
	}
	_, e = btcec.ParsePubKey(b)
	return e == nil
}
func Bounty(amount, bps int64) int64 { return (amount*bps + 9999) / 10000 }
func Digest(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Request struct {
	ID         string              `json:"id"`
	OfferEvent nostr.Event         `json:"offer_event"`
	Taker      string              `json:"taker"`
	Hash       string              `json:"hash"`
	Keys       map[chain.ID]string `json:"keys"`
}

func (r Request) Validate(now int64) (Offer, error) {
	o, e := DecodeOffer(r.OfferEvent, now)
	if e != nil {
		return o, e
	}
	if !Hex32(r.ID) || !Hex32(r.Hash) || !Hex32(r.Taker) || r.Taker == o.Maker || !ValidKey(r.Keys[chain.BTC]) || !ValidKey(r.Keys[chain.Blake]) || len(r.Keys) != 2 || r.Keys[chain.BTC] == r.Keys[chain.Blake] || o.Status != "open" {
		return o, errors.New("invalid swap request")
	}
	return o, nil
}

type Terms struct {
	Version      int                 `json:"version"`
	Request      Request             `json:"request"`
	MakerKeys    map[chain.ID]string `json:"maker_keys"`
	Long         contract.HTLC       `json:"long"`
	Short        contract.HTLC       `json:"short"`
	Takeover     uint32              `json:"takeover"`
	RevealBefore uint32              `json:"reveal_before"`
	Tower        string              `json:"tower"`
	TowerScripts map[chain.ID]string `json:"tower_scripts"`
	Domains      map[chain.ID]string `json:"domains"`
}

func NewTerms(r Request, makerKeys map[chain.ID]string, heights map[chain.ID]uint32, tower string, scripts map[chain.ID]string) (Terms, error) {
	o, e := r.Validate(time.Now().Unix())
	if e != nil {
		return Terms{}, e
	}
	longID := o.Sell.Other()
	shortID := o.Sell
	terms := Terms{Version: 1, Request: r, MakerKeys: makerKeys, Takeover: heights[longID] + TakeoverBlocks, RevealBefore: heights[longID] + RevealBlocks, Tower: tower, TowerScripts: scripts, Domains: map[chain.ID]string{chain.BTC: chain.BTC.Domain(), chain.Blake: chain.Blake.Domain()}}
	terms.Long = contract.HTLC{Chain: longID, Hash: r.Hash, ClaimKey: makerKeys[longID], RefundKey: r.Keys[longID], RefundHeight: heights[longID] + LongBlocks, Amount: o.BuyAmount}
	terms.Short = contract.HTLC{Chain: shortID, Hash: r.Hash, ClaimKey: r.Keys[shortID], RefundKey: makerKeys[shortID], RefundHeight: heights[shortID] + ShortBlocks, Amount: o.SellAmount}
	return terms, terms.Validate()
}
func (t Terms) Validate() error {
	// Expiration is a negotiation gate, not an excuse to forget funded contracts.
	o, e := t.Request.Validate(int64(t.Request.OfferEvent.CreatedAt))
	if e != nil {
		return e
	}
	if t.Version != 1 || !Hex32(t.Request.ID) || !Hex32(t.Request.Taker) || !Hex32(t.Request.Hash) || len(t.MakerKeys) != 2 || len(t.Request.Keys) != 2 {
		return errors.New("invalid terms")
	}
	for _, id := range []chain.ID{chain.BTC, chain.Blake} {
		if !ValidKey(t.MakerKeys[id]) || !ValidKey(t.Request.Keys[id]) || t.Domains[id] != id.Domain() {
			return errors.New("key/domain mismatch")
		}
		if o.TowerBPS > 0 {
			s, e := hex.DecodeString(t.TowerScripts[id])
			if !Hex32(t.Tower) || e != nil || len(s) != 22 || s[0] != 0 || s[1] != 20 {
				return errors.New("invalid tower payout")
			}
		}
	}
	if t.Long.Chain != o.Sell.Other() || t.Short.Chain != o.Sell || t.Long.Hash != t.Request.Hash || t.Short.Hash != t.Request.Hash || t.Long.ClaimKey != t.MakerKeys[t.Long.Chain] || t.Long.RefundKey != t.Request.Keys[t.Long.Chain] || t.Short.ClaimKey != t.Request.Keys[t.Short.Chain] || t.Short.RefundKey != t.MakerKeys[t.Short.Chain] || t.Long.Amount != o.BuyAmount || t.Short.Amount != o.SellAmount || t.Long.TxID != "" || t.Short.TxID != "" || t.Long.Vout != 0 || t.Short.Vout != 0 {
		return errors.New("contract differs from agreed order/keys")
	}
	if _, e = t.Long.Script(); e != nil {
		return e
	}
	if _, e = t.Short.Script(); e != nil {
		return e
	}
	if t.Long.RefundHeight < LongBlocks || t.Short.RefundHeight < ShortBlocks || t.Takeover != t.Long.RefundHeight-LongBlocks+TakeoverBlocks || t.RevealBefore != t.Long.RefundHeight-LongBlocks+RevealBlocks {
		return errors.New("invalid deadline schedule")
	}
	return nil
}

// Phase checks use each chain's own height, never compare cross-chain heights.
// The 2:1 horizon assumes similar progress; divergent chain rates remain a risk.
func (t Terms) Gate(phase string, heights map[chain.ID]uint32) error {
	if heights[t.Long.Chain] == 0 || heights[t.Short.Chain] == 0 {
		return errors.New("both chain heights are required")
	}
	lh, sh := heights[t.Long.Chain], heights[t.Short.Chain]
	if lh >= t.Long.RefundHeight || sh >= t.Short.RefundHeight {
		return errors.New("refund horizon reached")
	}
	longLeft, shortLeft := t.Long.RefundHeight-lh, t.Short.RefundHeight-sh
	switch phase {
	case "fund-long":
		if longLeft < 84 || shortLeft < 40 {
			return errors.New("acceptance too old to fund")
		}
	case "fund-short":
		if longLeft < 64 || shortLeft < 32 || lh+8 > t.RevealBefore {
			return errors.New("insufficient funding safety margin")
		}
	case "reveal":
		if longLeft < 48 || shortLeft < 16 || lh >= t.RevealBefore {
			return errors.New("secret reveal cutoff reached; wait for refunds")
		}
	default:
		return errors.New("unknown safety gate")
	}
	return nil
}
func (t Terms) Offer() Offer {
	var o Offer
	_ = json.Unmarshal([]byte(t.Request.OfferEvent.Content), &o)
	return o
}
func (t Terms) Party(role string) string {
	if role == "maker" {
		return t.Offer().Maker
	}
	return t.Request.Taker
}
func (t Terms) String() string {
	return fmt.Sprintf("%s %s/%s", t.Request.ID, t.Long.Chain, t.Short.Chain)
}
