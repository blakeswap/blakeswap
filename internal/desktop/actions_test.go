package desktop

import (
	"context"
	"encoding/json"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"testing"
	"time"
)

func TestActionSummaryAllWalletsAndOpeningNotSelectedPage(t *testing.T) {
	now := time.Now().Unix()
	raw := func(id string, actions []daemon.WalletAction) json.RawMessage {
		b, _ := json.Marshal(daemon.Status{Actions: daemon.WalletActions{WalletID: id, Network: chain.Regtest, Known: true, ObservedAt: now, Source: "live", Actions: actions}})
		return b
	}
	m := &Manager{}
	view := &desktopView{settings: &pb.Settings{ActiveNetwork: "regtest", Revision: 8, Wallets: []*pb.WalletProfile{{Id: "selected"}, {Id: "tower"}, {Id: "opening"}}}, statuses: map[string]json.RawMessage{"selected": raw("selected", nil), "tower": raw("tower", []daemon.WalletAction{{ID: "tower/accepted", Kind: "tower", State: "tower_monitoring", RequiresMonitoring: true}})}, workers: map[string]*walletWorker{}}
	m.view.Store(view)
	result, err := m.command(context.Background(), "selected", daemon.Request{Method: "actions.summary", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	s := result.(daemon.ActionSummary)
	if len(s.Wallets) != 3 || s.Complete || !s.RequiresMonitoring || s.SettingsRevision != 8 {
		t.Fatal(s)
	}
	if len(s.Wallets[1].Actions) != 1 || s.Wallets[2].Known {
		t.Fatal("accepted job or unknown wallet hidden", s)
	}
}
func TestActionSummaryCommandBoundaryAndNonblockingRefresh(t *testing.T) {
	now := time.Now().Unix()
	m := &Manager{}
	raw, _ := json.Marshal(daemon.Status{Actions: daemon.WalletActions{WalletID: "a", Network: chain.Regtest, Known: true, Source: "live", ObservedAt: now}})
	snapshot := json.RawMessage(raw)
	w := &walletWorker{refresh: make(chan chan refreshResult, 1)}
	w.snapshot.Store(&snapshot)
	v := &desktopView{settings: &pb.Settings{ActiveNetwork: "regtest", Wallets: []*pb.WalletProfile{{Id: "a"}}}, workers: map[string]*walletWorker{"a": w}}
	m.view.Store(v)
	check := func(refresh bool) daemon.ActionSummary {
		raw, _ := json.Marshal(map[string]bool{"refresh": refresh})
		s, err := m.actionSummary(context.Background(), daemon.Request{Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := check(false); !s.Complete || s.RequiresMonitoring {
		t.Fatal(s)
	}
	m.actionCommands.Add(1)
	if s := check(false); s.Complete || !s.RequiresMonitoring {
		t.Fatal("queued authorization hidden", s)
	}
	m.actionCommands.Add(-1)
	w.busy.Add(1)
	if s := check(false); s.Complete {
		t.Fatal("inflight worker hidden", s)
	}
	w.busy.Add(-1)
	// No worker is consuming refreshes. Both reads must return immediately,
	// including when the bounded queue is full, without waiting for chain IO.
	for i := 0; i < 2; i++ {
		if s := check(true); !s.Complete {
			t.Fatal(s)
		}
	}
	if len(w.refresh) != 1 {
		t.Fatal("refresh was not queued")
	}
}
