package desktop

import (
	"context"
	"encoding/json"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"time"
)

func (m *Manager) actionSummary(ctx context.Context, req daemon.Request) (daemon.ActionSummary, error) {
	if err := ctx.Err(); err != nil {
		return daemon.ActionSummary{}, err
	}
	var request struct {
		Refresh bool `json:"refresh"`
	}
	if err := json.Unmarshal(req.Params, &request); err != nil {
		return daemon.ActionSummary{}, err
	}
	version := m.actionVersion.Load()
	v := m.view.Load()
	if v == nil {
		return daemon.ActionSummary{}, status.Error(codes.Unavailable, "wallet summary is opening")
	}
	network := chain.Network(v.settings.ActiveNetwork)
	wallets := []daemon.WalletActions{}
	for _, wallet := range v.settings.Wallets {
		w := daemon.WalletActions{WalletID: wallet.Id, Network: network}
		raw := v.statuses[wallet.Id]
		var workerVersion uint64
		if worker := v.workers[wallet.Id]; worker != nil {
			workerVersion = worker.actionVersion.Load()
			raw = worker.read()
			if request.Refresh {
				select {
				case worker.refresh <- make(chan refreshResult, 1):
				default:
				}
			}
		}
		var s daemon.Status
		if json.Unmarshal(raw, &s) == nil && s.Actions.WalletID == wallet.Id && s.Actions.Network == network {
			w = s.Actions
		}
		// A first-run wallet has no installed state yet. Connecting saved wallets
		// always remain unknown until their authoritative worker has loaded state.
		if worker := v.workers[wallet.Id]; worker != nil && (worker.busy.Load() > 0 || worker.actionVersion.Load() != workerVersion) {
			w.Known = false
		}
		if v.settings.OnboardingStage == "wallet" {
			w.Known = true
			w.ObservedAt = time.Now().Unix()
		}
		wallets = append(wallets, w)
	}
	if m.actionCommands.Load() > 0 || m.actionVersion.Load() != version || m.view.Load() != v {
		for i := range wallets {
			wallets[i].Known = false
		}
	}
	return daemon.SummarizeActions(network, v.settings.Revision, wallets, time.Now().Unix()), nil
}
