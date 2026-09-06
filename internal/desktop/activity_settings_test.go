package desktop

import "testing"

func TestActivityExplorerSettingsRemainExplicitAndNetworkScoped(t *testing.T) {
	s := Defaults()
	for _, e := range s.Environments {
		if len(e.Explorers) != 0 {
			t.Fatal("default guessed an explorer")
		}
	}
	for _, v := range []struct {
		network, url string
		valid        bool
	}{
		{"mainnet", "https://example.invalid/tx/{txid}", true}, {"regtest", "http://127.0.0.1:8000/tx/{txid}", true}, {"testnet", "http://127.0.0.1:8000/tx/{txid}", false},
		{"mainnet", "https://user:pass@example.invalid/{txid}", false}, {"mainnet", "https://example.invalid/{txid}?secret=token", false}, {"mainnet", "https://example.invalid/{txid}/{txid}", false}, {"mainnet", "javascript:{txid}", false}, {"mainnet", "", true},
	} {
		if err := validateExplorer(v.network, v.url); (err == nil) != v.valid {
			t.Fatal(v, err)
		}
	}
}
