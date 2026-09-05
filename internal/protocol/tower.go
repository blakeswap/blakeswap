package protocol

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
)

type Job struct {
	Network         chain.Network  `json:"network,omitempty"`
	ObserveScanFrom uint32         `json:"observe_scan_from,omitempty"`
	ID              string         `json:"id"`
	SwapID          string         `json:"swap_id"`
	Owner           string         `json:"owner"`
	TermsHash       string         `json:"terms_hash"`
	Kind            string         `json:"kind"`
	Target          contract.HTLC  `json:"target"`
	Observe         *contract.HTLC `json:"observe,omitempty"`
	ScanFrom        uint32         `json:"scan_from"`
	Lock            uint32         `json:"lock"`
	BPS             int64          `json:"bps"`
	Payout          string         `json:"payout"`
	TowerScript     string         `json:"tower_script"`
	Templates       []string       `json:"templates"`
}
type Receipt struct {
	JobID  string `json:"job_id"`
	Digest string `json:"digest"`
}

func (j Job) Validate(towerScripts map[chain.ID]string, bps int64) error {
	if !j.Network.Valid() || !Hex32(j.ID) || !Hex32(j.SwapID) || !Hex32(j.Owner) || !Hex32(j.TermsHash) || !Hex32(j.Target.TxID) || j.Target.Vout != 0 || j.ScanFrom < 1 || j.BPS != bps || bps <= 0 || bps > 1000 || j.TowerScript != towerScripts[j.Target.Chain] {
		return errors.New("invalid tower job identity/quote")
	}
	if j.Network.Normalized() != chain.Regtest {
		if j.ScanFrom < j.Network.ForkHeight() || j.Target.RefundHeight < TimeLockThreshold || j.Lock < TimeLockThreshold || (j.Observe != nil && (j.Observe.RefundHeight < TimeLockThreshold || j.ObserveScanFrom < j.Network.ForkHeight())) {
			return errors.New("invalid public tower timing")
		}
	}
	refund := j.Kind == "refund"
	if !refund && j.Kind != "claim" {
		return errors.New("invalid tower job kind")
	}
	if refund {
		if j.Lock != j.Target.RefundHeight+RefundDelay(j.Network) || j.Observe != nil {
			return errors.New("refund grace mismatch")
		}
	} else {
		if j.Target.RefundHeight < 32 || j.Lock >= j.Target.RefundHeight-16 || j.Observe == nil || j.Observe.Chain != j.Target.Chain.Other() || j.Observe.Hash != j.Target.Hash || !Hex32(j.Observe.TxID) || j.Observe.Vout != 0 {
			return errors.New("invalid claim observation/rescue margin")
		}
		if _, e := j.Observe.Script(); e != nil {
			return e
		}
	}
	payout, e := hex.DecodeString(j.Payout)
	if e != nil || len(payout) != 22 || payout[0] != 0 || payout[1] != 20 {
		return errors.New("invalid owner payout")
	}
	tower, e := hex.DecodeString(j.TowerScript)
	if e != nil || len(tower) != 22 {
		return errors.New("invalid tower script")
	}
	bounty := Bounty(j.Target.Amount, j.BPS)
	if bounty < contract.Dust || len(j.Templates) != len(RescueFees) {
		return errors.New("uneconomic job/fee ladder")
	}
	for i, raw := range j.Templates {
		tx, e := contract.Parse(raw)
		if e != nil {
			return e
		}
		if e = contract.VerifySignature(j.Target, tx, refund); e != nil {
			return e
		}
		if tx.LockTime != j.Lock || tx.Version != 2 || len(tx.TxOut) != 2 || tx.TxOut[0].Value != j.Target.Amount-bounty-RescueFees[i] || tx.TxOut[0].Value < contract.Dust || !bytes.Equal(tx.TxOut[0].PkScript, payout) || tx.TxOut[1].Value != bounty || !bytes.Equal(tx.TxOut[1].PkScript, tower) {
			return fmt.Errorf("unsafe tower template %d", i)
		}
		if !refund && len(tx.TxIn[0].Witness[1]) != 0 {
			return errors.New("tower must never receive a private preimage")
		}
	}
	return nil
}
