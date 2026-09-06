package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/btcec/v2"
)

type BumpRequest struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Fee          int64  `json:"fee"`
	ExpectedTxID string `json:"expected_txid"`
}
type BumpResult struct {
	TxID  string `json:"txid"`
	Fee   int64  `json:"fee"`
	State string `json:"state"`
	Error string `json:"error"`
}

func transactionIDs(raws []string) []string {
	ids := []string{}
	for _, raw := range raws {
		if tx, err := contract.Parse(raw); err == nil {
			ids = append(ids, tx.TxHash().String())
		}
	}
	return ids
}

func (e *Engine) bumpTransaction(ctx context.Context, raw json.RawMessage) (BumpResult, error) {
	var p BumpRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return BumpResult{}, err
	}
	if p.Kind == "funding" {
		return BumpResult{}, errors.New("funding acceleration is unsupported: replacing its transaction ID invalidates the signed refunds, peer commitments and tower jobs; retain the original funding transaction and recovery bundle")
	}
	if p.Kind == "send" {
		return e.bumpSend(ctx, p)
	}
	if p.Kind != "claim" && p.Kind != "refund" {
		return BumpResult{}, errors.New("unsupported transaction kind")
	}
	s := e.s.Swaps[p.ID]
	if s == nil || s.Terms == nil {
		return BumpResult{}, errors.New("unknown funded swap")
	}
	if p.Kind == "claim" && s.OwnerFeeCap == 0 {
		return BumpResult{}, errors.New("legacy claim has no authorized replacement budget; retain its signed transaction")
	}
	if err := e.prepareClaimVariants(s); err != nil {
		return BumpResult{}, err
	}
	variants := s.SelfClaims
	target := s.Short
	attempt := &s.ClaimAttempt
	last := &s.ClaimLastAttempt
	if s.Role == "maker" {
		target = s.Long
	}
	if p.Kind == "refund" {
		variants = s.SelfRefunds
		target = s.Long
		attempt = &s.RefundAttempt
		last = &s.RefundLastAttempt
		if s.Role == "maker" {
			target = s.Short
		}
		if !e.eligible(target.Chain, target.RefundHeight) {
			return BumpResult{}, errors.New("refund is not eligible yet")
		}
	}
	if len(variants) == 0 {
		return BumpResult{}, errors.New("no signed transaction is eligible for acceleration")
	}
	index := -1
	for i, raw := range variants {
		tx, err := contract.Parse(raw)
		if err != nil {
			return BumpResult{}, err
		}
		if target.Amount-tx.TxOut[0].Value == p.Fee {
			index = i
		}
	}
	if index < 0 {
		return BumpResult{}, errors.New("fee must match an existing authorized settlement variant (2000, 6000, or 20000 sats)")
	}
	current := s.ClaimVariant
	if p.Kind == "refund" {
		current = s.RefundVariant
	}
	if current >= len(variants) {
		current = len(variants) - 1
	}
	tx, _ := contract.Parse(variants[index])
	if index < current {
		return BumpResult{}, errors.New("fee cannot decrease")
	}
	if index > current {
		old, _ := contract.Parse(variants[current])
		if p.ExpectedTxID != old.TxHash().String() {
			return BumpResult{}, errors.New("transaction changed; refresh before accelerating")
		}
	}
	// An unavailable backend is not proof of absence. Check every variant before
	// committing another, and never drop old raw transactions or reservations.
	for _, raw := range variants {
		v, _ := contract.Parse(raw)
		known, err := e.nodes[target.Chain].Transaction(ctx, v.TxHash().String())
		if err != nil && !chain.TransactionNotFound(err) {
			return BumpResult{}, err
		}
		if err == nil && known.Confirmations > 0 {
			return BumpResult{}, errors.New("settlement is already confirmed; refresh status")
		}
	}
	if index > current {
		*attempt = index * 3
	}
	if p.Kind == "refund" {
		s.RefundVariant = index
	} else {
		s.ClaimVariant = index
	}
	*last = time.Now().Unix()
	if err := e.save(); err != nil {
		return BumpResult{}, err
	}
	result := BumpResult{TxID: tx.TxHash().String(), Fee: p.Fee, State: "broadcast"}
	if err := e.broadcast(ctx, target.Chain, variants[index]); err != nil {
		result.State = "saved"
		result.Error = err.Error()
	}
	return result, e.save()
}

func (e *Engine) bumpSend(ctx context.Context, p BumpRequest) (BumpResult, error) {
	s := e.s.Sends[p.ID]
	if s == nil {
		return BumpResult{}, errors.New("unknown send")
	}
	if len(s.Coins) == 0 || s.MaxFee == 0 {
		return BumpResult{}, errors.New("legacy send has no saved replacement budget or signing inputs")
	}
	for _, v := range s.History {
		if v.Fee == p.Fee {
			return BumpResult{TxID: v.TxID, Fee: v.Fee, State: s.State, Error: s.Error}, nil
		}
	}
	if len(s.History) >= 16 {
		return BumpResult{}, errors.New("replacement limit reached; all signed variants remain monitored")
	}
	e.advanceSend(ctx, s)
	if s.State == "unknown" {
		return BumpResult{}, errors.New(s.Error)
	}
	if s.Confirmations > 0 {
		return BumpResult{}, errors.New("send is already confirmed")
	}
	if p.ExpectedTxID != s.TxID {
		return BumpResult{}, errors.New("transaction changed; refresh before accelerating")
	}
	if p.Fee <= s.Fee || p.Fee > s.MaxFee || p.Fee > feeLimits(s.Chain).Send {
		return BumpResult{}, errors.New("replacement fee must increase within the originally authorized maximum")
	}
	old, err := contract.Parse(s.Raw)
	if err != nil {
		return BumpResult{}, err
	}
	if len(old.TxOut) != 2 {
		return BumpResult{}, errors.New("send has no change available for a recipient-preserving replacement")
	}
	keys := map[string]*btcec.PrivateKey{}
	for _, entry := range e.receiveBook[s.Chain] {
		keys[hex.EncodeToString(entry.script)] = entry.key
	}
	tx, err := contract.PayWithKeys(s.Chain, s.Amount, old.TxOut[0].PkScript, s.Coins, keys, old.TxOut[1].PkScript, p.Fee)
	if err != nil {
		return BumpResult{}, err
	}
	if s.Chain == chain.BTC {
		if err := chain.ProveBTCExclusive(ctx, e.Config.Network, e.nodes[chain.BTC], e.nodes[chain.Blake], tx); err != nil {
			return BumpResult{}, err
		}
	}
	// A conservative 1 native sat/vB increment covers the supported nodes'
	// default incremental relay policies; nodes may still demand a higher fee.
	if p.Fee-s.Fee < contract.VirtualSize(tx) {
		return BumpResult{}, errors.New("replacement fee increase must cover at least one native satoshi per virtual byte")
	}
	v := SignedVariant{PublicVariant: PublicVariant{TxID: tx.TxHash().String(), Fee: p.Fee}, Raw: contract.Hex(tx)}
	s.History = append(s.History, v)
	s.Raw = v.Raw
	s.TxID = v.TxID
	s.Fee = p.Fee
	s.Change = tx.TxOut[1].Value
	s.LastAttempt = 0
	s.Submitted = false
	s.State = "saved"
	if err := e.save(); err != nil {
		return BumpResult{}, err
	}
	e.advanceSend(ctx, s)
	return BumpResult{TxID: s.TxID, Fee: s.Fee, State: s.State, Error: s.Error}, e.save()
}

func (e *Engine) prepareClaimVariants(s *Swap) error {
	if len(s.SelfClaims) > 0 || s.SelfClaim == "" {
		return nil
	}
	variants := []string{s.SelfClaim}
	if s.OwnerFeeCap == 0 {
		s.SelfClaims = variants
		return nil
	} // Do not reinterpret a legacy signed claim.
	target := s.Short
	if s.Role == "maker" {
		target = s.Long
	}
	base, err := contract.Parse(s.SelfClaim)
	if err != nil {
		return err
	}
	key, err := e.swapKey(target.Chain, s.ID)
	if err != nil {
		return err
	}
	secret, err := hex.DecodeString(s.Secret)
	if err != nil {
		return err
	}
	for _, fee := range protocol.RescueFees[1:] {
		if fee > s.OwnerFeeCap {
			break
		}
		tx, err := contract.Spend(target, key, base.TxOut[0].PkScript, fee, false, base.LockTime, nil, 0, secret)
		if err != nil {
			return err
		}
		variants = append(variants, contract.Hex(tx))
	}
	s.SelfClaims = variants
	return nil
}

// New swaps consent to a bounded owner ladder before funding. Legacy claims
// keep their base transaction; legacy refunds retain their signed variants but
// automatic escalation requires the new persisted policy.
func (e *Engine) broadcastOwner(ctx context.Context, s *Swap, id chain.ID, refund bool) error {
	if err := e.prepareClaimVariants(s); err != nil {
		return err
	}
	variants := s.SelfClaims
	attempt := &s.ClaimAttempt
	last := &s.ClaimLastAttempt
	if refund {
		variants = s.SelfRefunds
		attempt = &s.RefundAttempt
		last = &s.RefundLastAttempt
	}
	if len(variants) == 0 {
		return errors.New("missing signed settlement")
	}
	if time.Now().Unix()-*last < 30 {
		return nil
	}
	index := *attempt / 3
	if s.OwnerFeeCap == 0 {
		index = s.ClaimVariant
		if refund {
			index = s.RefundVariant
		}
	}
	if index >= len(variants) {
		index = len(variants) - 1
	}
	if s.OwnerFeeCap > 0 {
		// Estimates select only among the owner's already-authorized templates.
		feeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		suggested := estimatedTier(e.estimateFee(feeCtx, id, 2), variants)
		cancel()
		if suggested > index {
			index = suggested
			*attempt = index * 3
		}
	}
	if refund {
		s.RefundVariant = index
	} else {
		s.ClaimVariant = index
	}
	*last = time.Now().Unix()
	if s.OwnerFeeCap > 0 && *attempt < len(variants)*3 {
		*attempt++
	}
	if err := e.save(); err != nil {
		return err
	}
	return e.broadcast(ctx, id, variants[index])
}

func estimatedTier(estimate chain.FeeEstimate, variants []string) int {
	if !estimate.Current(time.Now()) {
		return 0
	}
	for i, raw := range variants {
		tx, err := contract.Parse(raw)
		if err != nil {
			return 0
		}
		// A withheld tower preimage still occupies 32 bytes when broadcast.
		if len(tx.TxIn) == 1 && len(tx.TxIn[0].Witness) == 4 && len(tx.TxIn[0].Witness[1]) == 0 {
			tx.TxIn[0].Witness[1] = make([]byte, 32)
		}
		fee, err := contract.FeeForVSize(estimate.Rate, contract.VirtualSize(tx))
		if err != nil {
			return 0
		}
		if i < len(protocol.RescueFees) && protocol.RescueFees[i] >= fee {
			return i
		}
	}
	return len(variants) - 1
}

func settlementVariant(raws []string, index int, long, short contract.HTLC, useLong bool) (string, int64) {
	if index < 0 || index >= len(raws) {
		return "", 0
	}
	tx, err := contract.Parse(raws[index])
	if err != nil || len(tx.TxOut) == 0 {
		return "", 0
	}
	amount := short.Amount
	if useLong {
		amount = long.Amount
	}
	return tx.TxHash().String(), amount - tx.TxOut[0].Value
}
