package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestAutomationInvalidDurableStateRejectedBeforeOpenOrRecovery(t *testing.T) {
	for _, mode := range []string{"nil-policy", "nil-charge-map", "nil-charge", "mismatched-policy", "mismatched-charge", "negative-charge", "unknown-charge", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			s := State{Version: StateVersion, Network: chain.Regtest, Mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about", Automations: map[string]*AutomationPolicy{"policy": {Config: AutomationConfig{ID: "policy"}, Charges: map[string]*AutomationCharge{"offer": {OfferID: "offer", State: "reserved", Volume: 100000, BTCFees: 20000, BlakeFees: 22000}}}}}
			p := s.Automations["policy"]
			switch mode {
			case "nil-policy":
				s.Automations["policy"] = nil
			case "nil-charge-map":
				p.Charges = nil
			case "nil-charge":
				p.Charges["offer"] = nil
			case "mismatched-policy":
				p.Config.ID = "other"
			case "mismatched-charge":
				p.Charges["offer"].OfferID = "other"
			case "negative-charge":
				p.Charges["offer"].Volume = -1
			case "unknown-charge":
				p.Charges["offer"].State = "forgotten"
			case "overflow":
				p.Charges["other"] = &AutomationCharge{OfferID: "other", State: "committed", Volume: contract.MaxMoney}
			}
			before, _ := json.Marshal(s)
			if err := ValidateAutomationState(&s); err == nil {
				t.Fatal("invalid records accepted")
			}
			if err := PrepareRecovery(&s, 100, false); err == nil {
				t.Fatal("invalid records admitted to recovery")
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("rejection changed obligations")
			}
			root := t.TempDir()
			password := []byte("isolated malformed state password")
			passwordPath := filepath.Join(root, "vault.password")
			if err := os.WriteFile(passwordPath, password, 0600); err != nil {
				t.Fatal(err)
			}
			vault, err := storage.Open(filepath.Join(root, "state.db"), password)
			if err != nil {
				t.Fatal(err)
			}
			if err = vault.Save(s); err != nil {
				t.Fatal(err)
			}
			if err = vault.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Name: "alice", Mode: "trader", Network: chain.Regtest, DataDir: root, PasswordFile: passwordPath, Relays: []string{"ws://127.0.0.1:1"}, Nodes: map[chain.ID]NodeConfig{}}
			for _, id := range []chain.ID{chain.BTC, chain.Blake} {
				cfg.Nodes[id] = NodeConfig{Kind: "rpc", URL: "http://127.0.0.1:1", Cookie: filepath.Join(root, "absent-cookie")}
			}
			engine, err := Open(context.Background(), cfg)
			if engine != nil {
				engine.Close()
				t.Fatal("invalid state opened")
			}
			if err == nil || !strings.Contains(err.Error(), "automation") {
				t.Fatal("invalid state did not return its validation error", err)
			}
			vault, err = storage.Open(filepath.Join(root, "state.db"), password)
			if err != nil {
				t.Fatal("load rejection leaked vault lock", err)
			}
			defer vault.Close()
			var saved State
			if _, err = vault.Load(&saved); err != nil {
				t.Fatal(err)
			}
			after, _ = json.Marshal(saved)
			if !bytes.Equal(before, after) {
				t.Fatal("load rejection altered durable state")
			}
		})
	}
}
