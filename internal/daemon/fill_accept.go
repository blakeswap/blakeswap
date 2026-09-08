package daemon

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// fillIdentityKeys binds both parties' per-child keys and the hash across active
// and cold history. Returning quantity never makes these identities reusable.
func fillIdentityKeys(request protocol.Request, makerKeys map[chain.ID]string) ([]string, error) {
	keys := []string{"hash/" + request.Hash}
	seen := map[string]bool{}
	for _, party := range []map[chain.ID]string{request.Keys, makerKeys} {
		for _, id := range []chain.ID{chain.BTC, chain.Blake} {
			key := party[id]
			if !protocol.ValidKey(key) || seen[key] {
				return nil, errors.New("fill requires distinct fresh keys for every party and chain")
			}
			seen[key] = true
			keys = append(keys, "key/"+key)
		}
	}
	return keys, nil
}

func (e *Engine) checkFillIdentities(id string, keys []string) error {
	for _, key := range keys {
		previous := e.s.FillKeys[key]
		if previous == "" {
			if _, err := e.archivedValue("fill_keys", key, &previous); err != nil {
				return err
			}
		}
		if previous != "" {
			return errors.New("fill hash or key was already assigned; exact retries require their retained child")
		}
	}
	return nil
}

func (e *Engine) acceptFillRequest(from string, m transport.Message) error {
	return e.acceptFillRequestAt(from, m, time.Now().Unix())
}

func (e *Engine) acceptFillRequestAt(from string, m transport.Message, now int64) error {
	if e.fatal != nil {
		return e.fatal
	}
	var request protocol.Request
	if err := json.Unmarshal(m.Body, &request); err != nil {
		return err
	}
	// Validate the retained signed identity before inspecting a retry. Expiry
	// gates a new allocation, not recognition of an already saved exact request.
	o, err := request.Validate(int64(request.OfferEvent.CreatedAt))
	if err != nil {
		return err
	}
	if o.Network.Normalized() != e.Config.Network || from != request.Taker || request.ID != m.SwapID || o.Maker != e.identity.Public().Hex() {
		return errors.New("request network or party mismatch")
	}
	existing := e.s.Swaps[request.ID]
	var cold Swap
	if existing == nil {
		if found, err := e.archivedValue("swaps", request.ID, &cold); err != nil {
			return err
		} else if found {
			existing = &cold
		}
	}
	if existing != nil {
		if existing.Role != "maker" || protocol.Digest(existing.Request) != protocol.Digest(request) {
			return errors.New("swap ID collision")
		}
		child := e.s.FillRecords[request.ID]
		var retained FillRecord
		if child == nil {
			if found, err := e.archivedValue("fill_records", request.ID, &retained); err != nil {
				return err
			} else if found {
				child = &retained
			}
		}
		if child == nil || child.RequestDigest != protocol.Digest(request) {
			return errors.New("saved child allocation identity is unavailable")
		}
		if child.FundingDisabled || child.Allocation.Disposition == FillRetired || terminalSwap(existing) {
			return e.queue(from, "rejected", request.ID, map[string]string{"reason": "child is closed; its original quantity is retained for history"})
		}
		if e.restoredSwap(request.ID) || child.ImportedUncertain {
			return errors.New("restored negotiations are quarantined; waiting for positive recovery evidence")
		}
		if existing.Terms == nil {
			return errors.New("saved maker acceptance has no terms")
		}
		return e.queue(from, "accepted", request.ID, existing.Terms)
	}
	if e.s.FillRecords[request.ID] != nil {
		return errors.New("retained child allocation has no matching core; new admission refused")
	}
	var retained FillRecord
	if found, err := e.archivedValue("fill_records", request.ID, &retained); err != nil {
		return err
	} else if found {
		return errors.New("retained child allocation has no matching core; new admission refused")
	}
	if _, err := request.Validate(now); err != nil {
		return err
	}
	if err := e.recoveryTradingReady(); err != nil {
		return err
	}
	parent := e.s.ParentOrders[o.ID]
	owned, ok := e.s.Offers[o.ID]
	if parent == nil || !ok || parent.RestoreHold || parent.Quantities.Closed || parent.SignedRevision != parent.Quantities.Revision || owned.ID != request.OfferEvent.ID || e.automationOfferHeld(o.ID) || e.strategyOfferHeld(o) {
		return e.queue(from, "rejected", request.ID, map[string]string{"reason": "order is unavailable or changed"})
	}
	if !e.fresh(chain.BTC) || !e.fresh(chain.Blake) {
		return errors.New("both chains require fresh observations before accepting a new request")
	}
	if err := e.admitWork("swap"); err != nil {
		return err
	}
	next, child, err := parent.reserveFill(request)
	if err != nil {
		return err
	}
	reservation, remaining, err := e.fillReservationCandidate(o.ID, o.Sell, request.Quantity+child.FundingPolicy.FundingFee)
	if err != nil {
		return err
	}
	child.Inputs = reservation.Inputs
	if policy := child.FundingPolicy; policy.Rate > 0 {
		vsize, err := contract.PaymentVSize(len(child.Inputs), make([]byte, 34), e.scripts[o.Sell])
		if err != nil {
			return err
		}
		if err := validateFeeRate(policy.Rate, policy.Timestamp, policy.FundingFee, vsize); err != nil {
			return err
		}
	}
	makerKeys, err := e.swapKeys(request.ID)
	if err != nil {
		return err
	}
	identityKeys, err := fillIdentityKeys(request, makerKeys)
	if err != nil {
		return err
	}
	if err := e.checkFillIdentities(request.ID, identityKeys); err != nil {
		return err
	}
	terms, err := protocol.NewTermsWithClocks(request, makerKeys, e.heights, e.clocks)
	if err != nil {
		return err
	}
	tower, ok := e.s.OfferTowers[o.ID]
	if !ok {
		return errors.New("local parent protection policy is missing")
	}
	s := &Swap{ID: request.ID, Role: "maker", Protection: &tower, Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short, OwnerFeeCap: child.FundingPolicy.OwnerFeeCap, Receipts: map[string]protocol.Receipt{}, Stage: "awaiting taker funding"}
	raw, err := json.Marshal(terms)
	if err != nil {
		return err
	}
	delivery, err := e.prepareDelivery(from, "accepted", request.ID, raw)
	if err != nil {
		return err
	}
	next, event, err := e.prepareParentPublication(next, now)
	if err != nil {
		return err
	}
	// All fallible admission/signing work is complete. One durable transaction
	// owns the input transfer, monetary and quantity bins, terms and acceptance.
	// A failed commit stops protocol execution; no publisher can observe success.
	e.s.ParentOrders[o.ID] = &next
	if e.s.FillRecords == nil {
		e.s.FillRecords = map[string]*FillRecord{}
	}
	if e.s.FillKeys == nil {
		e.s.FillKeys = map[string]string{}
	}
	if e.s.Swaps == nil {
		e.s.Swaps = map[string]*Swap{}
	}
	if e.s.FundingFees == nil {
		e.s.FundingFees = map[string]FeeSelection{}
	}
	if e.s.Outbox == nil {
		e.s.Outbox = map[string]*Delivery{}
	}
	e.s.FillRecords[request.ID], e.s.Swaps[request.ID] = child, s
	for _, key := range identityKeys {
		e.s.FillKeys[key] = request.ID
	}
	e.s.CoinReservations["offer/"+o.ID] = remaining
	e.s.CoinReservations["swap/"+request.ID] = reservation
	e.s.FundingFees["swap/"+request.ID] = child.FundingPolicy
	e.s.Outbox[delivery.MessageID] = delivery
	if event != nil {
		e.stageOffer(parentPublicOffer(next), *event)
	}
	return e.save()
}
