package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// The local node launcher registers paths, never cookie contents. Resolve this
// afresh on every call so restarting nodes or using a different checkout works.
func regtestRegistryPath() string {
	if path := os.Getenv("BLAKESWAP_REGTEST_REGISTRY"); path != "" {
		return path
	}
	cache, _ := os.UserCacheDir()
	return filepath.Join(cache, "Blakeswap", "regtest-nodes.json")
}

type localNode struct {
	URL    string `json:"url"`
	Cookie string `json:"cookie"`
}

func (r *RPC) cookiePath(ctx context.Context) (string, error) {
	if r.Cookie != "" {
		return r.Cookie, nil
	}
	u, err := url.Parse(r.URL)
	if err != nil || r.Network != Regtest || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return "", errors.New("choose an RPC cookie file; automatic discovery is only available for local regtest nodes")
	}
	// Probe before reporting a cookie problem: a fresh installation commonly has
	// no local node at all, rather than a wrongly selected credential file.
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", fmt.Errorf("local regtest %s node is not reachable at %s. Start it with make regtest-%s (or make regtest-nodes for both chains), then retry: %w", r.ID, r.URL, r.ID, err)
	}
	conn.Close()
	raw, err := os.ReadFile(regtestRegistryPath())
	if err == nil {
		var nodes map[ID]localNode
		if err := json.Unmarshal(raw, &nodes); err != nil {
			return "", fmt.Errorf("invalid local regtest registration: %w", err)
		}
		if node, ok := nodes[r.ID]; ok && node.URL == r.URL && filepath.IsAbs(node.Cookie) {
			return node.Cookie, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read local regtest registration: %w", err)
	}
	return "", fmt.Errorf("local regtest %s endpoint is reachable at %s, but its RPC cookie is not registered. Run make regtest-%s for managed local nodes, or choose this node's cookie file in Settings", r.ID, r.URL, r.ID)
}
