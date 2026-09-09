package api

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
)

func TestAutomationTypedAuthorizationAndExactBudget(t *testing.T) {
	config := daemon.AutomationConfig{ID: "policy", Wallet: "alice", Network: chain.Regtest, Sell: chain.BTC, SellAmount: 9999999999, VolumeLimit: 2100000000000000, Rate: daemon.AutomationRate{Numerator: 2100000000000000, Denominator: 2099999999999999}, Reference: "orderbook", ReferenceMakers: []string{"one", "two", "three"}, ReferenceFreshness: 120, ReferenceSpreadBPS: 30}
	usage := daemon.AutomationUsage{CommittedVolume: 10000000000, ReservedVolume: 9999999999, CommittedBTCFees: 20000, ReservedBlakeFees: 30000}
	service := Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		switch r.Method {
		case "automation.review", "automation.save":
			var p daemon.AutomationEdit
			if err := json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.Config.VolumeLimit != config.VolumeLimit || p.Config.Rate != config.Rate || p.ExpectedRevision != 7 || !p.Enabled || !p.AcknowledgeRestoredBudget {
				t.Fatal("lost exact authorization", p)
			}
			if r.Method == "automation.review" {
				return daemon.AutomationReview{Config: config, ExpectedRevision: 7, Enabled: true, ReviewDigest: "review", Usage: usage, Warning: "bounded"}, nil
			}
			if p.ReviewDigest != "review" {
				t.Fatal("save omitted review digest")
			}
			return daemon.AutomationView{Config: config, Revision: 8, Enabled: true, Usage: usage}, nil
		case "automation.list":
			return daemon.AutomationList{Wallet: "alice", Network: chain.Regtest, Policies: []daemon.AutomationView{{Config: config, Revision: 8, Enabled: true, RestoreHold: true, Usage: usage, Decision: "held", ReferenceEvents: []string{"event"}}}}, nil
		case "automation.disable":
			var q pb.DisableAutomationRequest
			if err := json.Unmarshal(r.Params, &q); err != nil {
				t.Fatal(err)
			}
			if q.ExpectedWallet != "alice" || q.ExpectedNetwork != "regtest" || q.ExpectedRevision != 8 || !q.CancelOpen {
				t.Fatal(&q)
			}
			return daemon.AutomationView{Config: config, Revision: 9, Decision: "disabled"}, nil
		default:
			t.Fatal(r.Method)
			return nil, nil
		}
	}}
	c := &pb.AutomationConfig{Id: config.ID, Wallet: config.Wallet, Network: string(config.Network), Sell: string(config.Sell), SellAmount: config.SellAmount, VolumeLimit: config.VolumeLimit, Rate: &pb.AutomationRate{Numerator: config.Rate.Numerator, Denominator: config.Rate.Denominator}}
	p := &pb.AutomationEdit{Config: c, ExpectedRevision: 7, Enabled: true, AcknowledgeRestoredBudget: true}
	q, err := service.ReviewAutomation(context.Background(), p)
	if err != nil || q.GetConfig().GetRate().GetNumerator() != config.Rate.Numerator || q.GetUsage().GetReservedVolume() != usage.ReservedVolume {
		t.Fatal(q, err)
	}
	p.ReviewDigest = q.ReviewDigest
	if saved, err := service.SaveAutomation(context.Background(), p); err != nil || saved.GetRevision() != 8 {
		t.Fatal(saved, err)
	}
	if list, err := service.ListAutomations(context.Background(), &pb.AutomationQuery{ExpectedWallet: "alice", ExpectedNetwork: "regtest"}); err != nil || !list.Policies[0].RestoreHold || list.Policies[0].ReferenceEvents[0] != "event" {
		t.Fatal(list, err)
	}
	if _, err := service.DisableAutomation(context.Background(), &pb.DisableAutomationRequest{Id: "policy", ExpectedWallet: "alice", ExpectedNetwork: "regtest", ExpectedRevision: 8, CancelOpen: true}); err != nil {
		t.Fatal(err)
	}
}
