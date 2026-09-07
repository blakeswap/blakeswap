package daemon

import (
	"bytes"
	"errors"
	"github.com/blakeswap/blakeswap/internal/storage"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// These values describe monitoring; they never authorize signing or publication.
// IDs are local navigation identities, not transaction IDs or secret material.
type ActionDeadline struct {
	Kind       string   `json:"kind"`
	Chain      chain.ID `json:"chain"`
	Unit       string   `json:"unit"`
	Target     uint32   `json:"target"`
	Observed   uint32   `json:"observed"`
	ObservedAt int64    `json:"observed_at"`
	Remaining  int64    `json:"remaining"`
	Certain    bool     `json:"certain"`
	Band       string   `json:"band"`
	Reason     string   `json:"reason"`
}
type WalletAction struct {
	ID                 string           `json:"id"`
	Kind               string           `json:"kind"`
	ObjectID           string           `json:"object_id"`
	State              string           `json:"state"`
	RequiresMonitoring bool             `json:"requires_monitoring"`
	Uncertain          bool             `json:"uncertain"`
	FirstReveal        bool             `json:"first_reveal"`
	TowerReady         bool             `json:"tower_ready"`
	Deadlines          []ActionDeadline `json:"deadlines"`
}
type WalletActions struct {
	Source     string         `json:"source"`
	WalletID   string         `json:"wallet_id"`
	Network    chain.Network  `json:"network"`
	Known      bool           `json:"known"`
	ObservedAt int64          `json:"observed_at"`
	Actions    []WalletAction `json:"actions"`
}
type ActionSummary struct {
	Network            chain.Network   `json:"network"`
	SettingsRevision   uint64          `json:"settings_revision"`
	ObservedAt         int64           `json:"observed_at"`
	Wallets            []WalletActions `json:"wallets"`
	Complete           bool            `json:"complete"`
	RequiresMonitoring bool            `json:"requires_monitoring"`
}

const ActionSnapshotMaxAge int64 = 90

func (e *Engine) actionDeadline(kind string, id chain.ID, target uint32, now int64) ActionDeadline {
	d := ActionDeadline{Kind: kind, Chain: id, Target: target, ObservedAt: e.chainObserved[id], Unit: "blocks", Observed: e.heights[id], Band: "unknown"}
	if target >= protocol.TimeLockThreshold {
		d.Unit = "median_time"
		d.Observed = e.clocks[id]
	}
	d.Remaining = int64(target) - int64(d.Observed)
	// Timestamp finality is strictly after MTP=locktime; reveal cutoffs are exclusive.
	if d.Unit == "median_time" && kind != "reveal" {
		d.Remaining++
	}
	d.Certain = e.fresh(id) && d.ObservedAt > 0 && now-d.ObservedAt <= ActionSnapshotMaxAge && now >= d.ObservedAt
	if kind == "reveal" {
		other := chain.BTC
		if id == other {
			other = chain.Blake
		}
		d.Certain = d.Certain && e.fresh(other) && e.chainObserved[other] > 0 && now >= e.chainObserved[other] && now-e.chainObserved[other] <= ActionSnapshotMaxAge
	}
	if d.Unit == "median_time" {
		d.Certain = d.Certain && int64(d.Observed) >= now-6*3600 && int64(d.Observed) <= now+2*3600
		if kind == "reveal" {
			other := chain.BTC
			if id == other {
				other = chain.Blake
			}
			diff := int64(e.clocks[id]) - int64(e.clocks[other])
			if diff < 0 {
				diff = -diff
			}
			d.Certain = d.Certain && e.fresh(other) && e.chainObserved[other] > 0 && now-e.chainObserved[other] <= ActionSnapshotMaxAge && int64(e.clocks[other]) >= now-6*3600 && int64(e.clocks[other]) <= now+2*3600 && diff <= int64(protocol.MaxClockSkew)
		}
	}
	if !d.Certain {
		d.Reason = "Current chain observations are unavailable or stale."
		return d
	}
	d.Band = "later"
	near := int64(6)
	if d.Unit == "median_time" {
		near = 2 * 3600
	}
	if d.Remaining <= 0 {
		d.Band = "reached"
	} else if d.Remaining <= near {
		d.Band = "approaching"
	}
	return d
}

// Called under e.mu. Keep this independent of paged UI history.
func (e *Engine) walletActions(now int64) WalletActions {
	w := WalletActions{WalletID: e.Config.Name, Network: e.Config.Network, Known: true, Source: "live", ObservedAt: now, Actions: []WalletAction{}}
	fresh := e.actionChainFresh(chain.BTC, now) && e.actionChainFresh(chain.Blake, now)
	held := map[string]bool{}
	if r := e.s.Recovery; r != nil {
		held = r.InvalidatedSettlements
		if r.Status.State != "ready" || len(held) > 0 {
			w.Actions = append(w.Actions, WalletAction{ID: "recovery", Kind: "recovery", State: "recovery_required", RequiresMonitoring: true, Uncertain: true})
		}
	}
	for id, event := range e.s.Offers {
		offer, err := protocol.DecodeOffer(event, now)
		if err == nil && (offer.Status == "open" || offer.Status == "reserved") {
			w.Actions = append(w.Actions, WalletAction{ID: "order/" + id, Kind: "order", ObjectID: id, State: "offer_open", RequiresMonitoring: true, Uncertain: !fresh})
		}
	}
	for id, s := range e.s.Swaps {
		if s == nil {
			w.Known = false
			continue
		}
		a := WalletAction{ID: "swap/" + id, Kind: "swap", ObjectID: id, State: "waiting_peer", RequiresMonitoring: !terminalSwap(s), Uncertain: !fresh, TowerReady: s.Terms != nil && s.protection().BPS > 0 && len(s.Jobs) > 0 && towerReady(s)}
		switch {
		case held[a.ID]:
			a.State = "reopened"
			a.RequiresMonitoring = true
			a.Uncertain = true
		case terminalSwap(s):
			a.State = "closed"
			a.Uncertain = false
			if s.Stage == "completed" {
				a.State = "completed"
			}
			if s.Stage == "refunded" {
				a.State = "refunded"
			}
		case s.Error != "":
			a.State = "attention"
		case s.Stage == "claiming":
			a.State = "owner_claim"
		case s.Stage == "refunding" || s.Stage == "awaiting refund deadline":
			a.State = "owner_refund"
		case s.LongFunding != "" || s.ShortFunding != "" || s.Long.TxID != "" || s.Short.TxID != "":
			a.State = "confirming"
		case s.Stage == "awaiting durable tower receipt":
			a.State = "tower_pending"
		}
		if a.RequiresMonitoring && s.Terms != nil {
			a.FirstReveal = s.Role == "taker" && !s.SecretExposed
			if a.FirstReveal {
				a.Deadlines = append(a.Deadlines, e.actionDeadline("reveal", s.Terms.Long.Chain, s.Terms.RevealBefore, now))
			}
			for _, leg := range []struct {
				chain chain.ID
				lock  uint32
			}{{s.Terms.Long.Chain, s.Terms.Long.RefundHeight}, {s.Terms.Short.Chain, s.Terms.Short.RefundHeight}} {
				a.Deadlines = append(a.Deadlines, e.actionDeadline("refund", leg.chain, leg.lock, now))
			}
			for _, d := range a.Deadlines {
				a.Uncertain = a.Uncertain || !d.Certain
			}
		}
		w.Actions = append(w.Actions, a)
	}
	for id, s := range e.s.Sends {
		if s == nil {
			w.Known = false
			continue
		}
		a := WalletAction{ID: "send/" + id, Kind: "send", ObjectID: id, State: "confirming", RequiresMonitoring: s.Confirmations < 6, Uncertain: !e.actionChainFresh(s.Chain, now)}
		if !a.RequiresMonitoring {
			a.State = "completed"
			a.Uncertain = false
		} else if s.Error != "" {
			a.State = "attention"
		} else if !s.Submitted {
			a.State = "signed_pending"
		}
		if held[a.ID] {
			a.RequiresMonitoring = true
			a.State = "reopened"
			a.Uncertain = true
		}
		w.Actions = append(w.Actions, a)
	}
	for id, j := range e.s.TowerJobs {
		if j == nil {
			w.Known = false
			continue
		}
		a := WalletAction{ID: "tower/" + id, Kind: "tower", ObjectID: id, State: "tower_monitoring", RequiresMonitoring: j.Confirmed < 6 && !j.Expired, Uncertain: !fresh, TowerReady: true}
		if !a.RequiresMonitoring {
			a.State = "closed"
			a.Uncertain = false
			if j.Confirmed >= 6 {
				a.State = "completed"
			}
		} else if j.Error != "" {
			a.State = "attention"
		} else if j.Broadcast != "" {
			a.State = "confirming"
		}
		if held[a.ID] {
			a.RequiresMonitoring = true
			a.State = "reopened"
			a.Uncertain = true
		}
		if a.RequiresMonitoring {
			a.Deadlines = append(a.Deadlines, e.actionDeadline("tower_"+j.Job.Kind, j.Job.Target.Chain, j.Job.Lock, now))
		}
		w.Actions = append(w.Actions, a)
	}
	sort.Slice(w.Actions, func(i, j int) bool { return w.Actions[i].ID < w.Actions[j].ID })
	return w
}

func SummarizeActions(network chain.Network, revision uint64, wallets []WalletActions, now int64) ActionSummary {
	s := ActionSummary{Network: network, SettingsRevision: revision, ObservedAt: now, Wallets: wallets, Complete: true}
	for i := range s.Wallets {
		w := &s.Wallets[i]
		if w.Network != network || w.ObservedAt <= 0 || now < w.ObservedAt || (w.Source != "stored" && now-w.ObservedAt > ActionSnapshotMaxAge) {
			w.Known = false
		}
		if !w.Known {
			s.Complete = false
			s.RequiresMonitoring = true
		}
		for j := range w.Actions {
			a := &w.Actions[j]
			s.RequiresMonitoring = s.RequiresMonitoring || a.RequiresMonitoring
			if !w.Known {
				a.Uncertain = true
			}
			for k := range a.Deadlines {
				d := &a.Deadlines[k]
				if !w.Known || now-d.ObservedAt > ActionSnapshotMaxAge || now < d.ObservedAt {
					d.Certain = false
					d.Band = "unknown"
					d.Reason = "Current chain observations are unavailable or stale."
					a.Uncertain = true
				}
			}
		}
	}
	return s
}
func (e *Engine) actionSummary() ActionSummary {
	now := time.Now().Unix()
	return SummarizeActions(e.Config.Network, 0, []WalletActions{e.walletActions(now)}, now)
}

// LoadStoredActions reads only encrypted local authority before a worker starts.
// No network call or key/address material enters the returned projection.
func LoadStoredActions(c Config) (WalletActions, error) {
	e := &Engine{Config: c}
	path := filepath.Join(c.DataDir, "state.db")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return WalletActions{}, err
		}
		password, err := os.ReadFile(c.PasswordFile)
		if err != nil {
			return WalletActions{}, err
		}
		defer clear(password)
		vault, err := storage.Open(path, bytes.TrimSpace(password))
		if err != nil {
			return WalletActions{}, err
		}
		defer vault.Close()
		if _, err = vault.Load(&e.s); err != nil {
			return WalletActions{}, err
		}
	}
	w := e.walletActions(time.Now().Unix())
	w.Source = "stored"
	return w, nil
}

func (e *Engine) actionChainFresh(id chain.ID, now int64) bool {
	return e.fresh(id) && e.chainObserved[id] > 0 && now >= e.chainObserved[id] && now-e.chainObserved[id] <= ActionSnapshotMaxAge
}
