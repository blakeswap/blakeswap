package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"os"
	"path/filepath"
	"time"
)

func (e *Engine) CanChangeNetwork() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return canChangeNetwork(e.s)
}
func canChangeNetwork(s State) error {
	for _, event := range s.Offers {
		var o protocol.Offer
		o, err := protocol.DecodeOffer(event, time.Now().Unix())
		if err == nil && (o.Status == "open" || o.Status == "reserved") {
			return errors.New("cancel open offers and finish active swaps before changing networks")
		}
	}
	for _, swap := range s.Swaps {
		switch swap.Stage {
		case "completed", "refunded", "rejected", "expired before funding", "aborted; counterparty refunded":
		default:
			return fmt.Errorf("swap %s must finish before changing networks", swap.ID)
		}
	}
	for _, job := range s.TowerJobs {
		if job.Confirmed < 6 && !job.Expired {
			return errors.New("watchtower jobs are still active")
		}
	}
	return nil
}

// CheckStoredNetwork reads the encrypted state when a node is unavailable, so a
// connection failure cannot bypass the active-swap network-switch guard.
func CheckStoredNetwork(c Config) error {
	path := filepath.Join(c.DataDir, "state.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	password, err := os.ReadFile(c.PasswordFile)
	if err != nil {
		return err
	}
	defer clear(password)
	vault, err := storage.Open(path, bytes.TrimSpace(password))
	if err != nil {
		return err
	}
	defer vault.Close()
	var s State
	if _, err = vault.Load(&s); err != nil {
		return err
	}
	return canChangeNetwork(s)
}

// CheckCommandNetwork prevents a stale UI or queued RPC from acting on a newly
// selected wallet. Standalone immutable-config daemons accept legacy callers.
func CheckCommandNetwork(req Request, actual chain.Network, required bool) error {
	switch req.Method {
	case "tower.resolve", "offer.create", "offer.cancel", "swap.take", "pause", "regtest.mine", "regtest.faucet":
	default:
		return nil
	}
	var binding struct {
		Network string `json:"expected_network"`
	}
	if err := json.Unmarshal(req.Params, &binding); err != nil {
		return err
	}
	if binding.Network == "" && !required {
		return nil
	}
	if binding.Network != string(actual.Normalized()) {
		return errors.New("trading network changed; refresh before submitting this action")
	}
	return nil
}
