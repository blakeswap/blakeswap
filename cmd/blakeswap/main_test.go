package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopCLIRequiresOwnedNativeConnectionOrExplicitFileMode(t *testing.T) {
	before := os.Args
	t.Cleanup(func() { os.Args = before })
	t.Setenv("BLAKESWAP_DESKTOP_SESSION", "")
	root := filepath.Join(t.TempDir(), "uncreated")
	os.Args = []string{"blakeswap", "desktop", "--data-dir", root}
	err := run()
	if err == nil || !strings.Contains(err.Error(), "app-owned security connection") {
		t.Fatal("desktop selected credentials without owned connection", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("failed default startup created installation")
	}
	os.Args = []string{"blakeswap", "desktop", "--credential-mode", "unexpected", "--data-dir", root}
	if err := run(); err == nil || !strings.Contains(err.Error(), "unknown credential mode") {
		t.Fatal("unknown mode accepted", err)
	}
}

func TestStandaloneCLIRequiresExplicitCredentialModeBeforeOpeningVault(t *testing.T) {
	before := os.Args
	t.Cleanup(func() { os.Args = before })
	path := filepath.Join(t.TempDir(), "config.json")
	for _, value := range []string{"{}", "{\"credential_mode\":\"native\"}"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		os.Args = []string{"blakeswap", "daemon", "--config", path}
		if err := run(); err == nil || !strings.Contains(err.Error(), "explicit credential_mode: file") {
			t.Fatal("standalone daemon implicitly selected a credential source", err)
		}
	}
}
