package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

func assertSettledActivity(t *testing.T, h *harness, id string) {
	t.Helper()
	for _, name := range []string{"maker", "taker"} {
		s := h.swap(name, id)
		kind := "claim"
		target := s.Long
		if s.Role == "taker" {
			target = s.Short
		}
		if s.Stage == "refunded" {
			kind = "refund"
			target = s.Short
			if s.Role == "taker" {
				target = s.Long
			}
		}
		key := "swap/" + id + "/" + kind
		h.until("confirmed activity "+name, func() bool { return h.engines[name].s.Activities[key].Status == "confirmed" }, func() { h.tick(name, "tower") })
		a := h.engines[name].s.Activities[key]
		tx, err := h.nodes[target.Chain].Transaction(h.ctx, a.TxID)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := contract.Parse(tx.Hex)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, out := range parsed.TxOut {
			total += out.Value
		}
		if a.Amount != parsed.TxOut[0].Value || a.Chain != target.Chain || a.Fee != target.Amount-total || !a.FeeKnown || !a.Movement || a.GroupID != "swap/"+id {
			t.Fatal("settlement activity economics", a)
		}
		funding := h.engines[name].s.Activities["swap/"+id+"/funding"]
		if !funding.Movement || funding.Direction != "outgoing" || !funding.FeeKnown {
			t.Fatal("funding activity missing", funding)
		}
		for _, receipt := range h.engines[name].s.Activities {
			if receipt.Kind == "receive" && receipt.GroupID == a.GroupID && receipt.Movement {
				t.Fatal("settlement receipt counted twice", receipt)
			}
		}
		raw, _ := json.Marshal(ActivityQuery{ExpectedWallet: name, ExpectedNetwork: string(h.engines[name].Config.Network)})
		page, err := h.engines[name].activityPage(raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(page)
		if s.Secret != "" && strings.Contains(string(encoded), s.Secret) {
			t.Fatal("activity exposed swap preimage")
		}
	}
	for jobID, job := range h.engines["tower"].s.TowerJobs {
		if job.Confirmed == 0 {
			continue
		}
		key := "tower/" + jobID
		h.until("tower earning activity", func() bool { return h.engines["tower"].s.Activities[key].Status == "confirmed" }, func() { h.tick("tower") })
		a := h.engines["tower"].s.Activities[key]
		if a.Amount != protocol.Bounty(job.Job.Target.Amount, job.Job.BPS) || !a.Movement || a.FeePayer != "contract_owner" {
			t.Fatal("wrong earned bounty", a, protocol.Bounty(job.Job.Target.Amount, job.Job.BPS))
		}
	}
}
