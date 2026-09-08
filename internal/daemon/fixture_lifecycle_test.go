package daemon

import (
	"maps"
	"testing"

	"github.com/blakeswap/blakeswap/internal/storage"
)

// A reopened vault starts a new engine with a freshly validated checkpoint.
// No failed engine's transaction, scratch index, pending archive moves or fatal
// state can be reused to manufacture a successful retry.
func reopenedFixtureEngine(t *testing.T, source *Engine, v *storage.Vault, saved State) *Engine {
	t.Helper()
	if err := ValidateVaultProtocolState(v, &saved); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Config: source.Config, s: saved, vault: v, keys: source.keys, identity: source.identity,
		nodes: maps.Clone(source.nodes), watch: maps.Clone(source.watch), scanners: source.scanners, towerScanners: source.towerScanners,
		addresses: maps.Clone(source.addresses), scripts: maps.Clone(source.scripts), heights: maps.Clone(source.heights), clocks: maps.Clone(source.clocks), balances: maps.Clone(source.balances),
		receiveBook: source.receiveBook, receiveReady: maps.Clone(source.receiveReady), walletCoins: source.walletCoins, walletCursor: maps.Clone(source.walletCursor),
		chainFresh: maps.Clone(source.chainFresh), chainObserved: maps.Clone(source.chainObserved), chainGeneration: maps.Clone(source.chainGeneration), chainErrors: maps.Clone(source.chainErrors),
		fillValidation: captureFillValidation(&saved, nil)}
	// Open completes an ordinary save before protocol/archive work begins.
	// This also seeds the fresh semantic and disposable custody checkpoints.
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e
}
