package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Called with the manager lock held (or before the run loop starts). Socket
// names stay short enough for macOS regardless of the stable wallet ID.
func (m *Manager) startAPI(profile string) error {
	service := &api.Service{Command: func(ctx context.Context, r daemon.Request) (any, error) { return m.command(ctx, profile, r) }, ReadSettings: m.readSettings, WriteSettings: m.writeSettings, NewWallet: m.createWallet}
	server, err := api.Listen(m.runtimeCtx, filepath.Join(m.runtimeDir, fmt.Sprintf("%d.sock", len(m.servers))), service)
	if err != nil {
		return err
	}
	m.servers[profile] = server
	return nil
}

func (m *Manager) writeRuntime() error {
	endpoints := map[string]api.Endpoint{}
	for id, server := range m.servers {
		endpoints[id] = server.Endpoint
	}
	raw, err := json.Marshal(endpoints)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(m.root, "runtime.json"), raw)
}

func (m *Manager) createWallet(ctx context.Context, request *pb.CreateWalletRequest) (*pb.Settings, error) {
	if err := validateWalletName(request.Name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.stopped {
		return nil, status.Error(codes.Unavailable, "daemon is stopping")
	}
	if request.Revision != m.settings.Revision {
		return nil, status.Error(codes.Aborted, "settings changed; reload before creating a wallet")
	}
	if len(m.settings.Wallets) >= 20 {
		return nil, status.Error(codes.ResourceExhausted, "at most 20 wallets")
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := "wallet-" + hex.EncodeToString(random[:])
	walletRoot := filepath.Join(m.root, "wallets")
	if err := os.MkdirAll(walletRoot, 0700); err != nil {
		return nil, err
	}
	walletDir := filepath.Join(walletRoot, id)
	if err := os.Mkdir(walletDir, 0700); err != nil {
		return nil, err
	}
	if _, _, err := master(walletDir); err != nil {
		return nil, err
	}
	if err := m.startAPI(id); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			m.servers[id].Close()
			delete(m.servers, id)
			_ = m.writeRuntime()
		}
	}()
	// Publish the endpoint before committing its profile. A crash before the
	// settings save leaves only an unused vault; startup rebuilds the manifest.
	if err := m.writeRuntime(); err != nil {
		return nil, err
	}
	saved := proto.Clone(m.settings).(*pb.Settings)
	saved.Wallets = append(saved.Wallets, &pb.WalletProfile{Id: id, Name: request.Name})
	saved.Revision++
	if err := saveSettings(m.root, saved); err != nil {
		return nil, err
	}
	m.settings = saved
	// The run loop bootstraps missing profiles without stopping active wallets.
	committed = true
	m.publishView()
	return proto.Clone(saved).(*pb.Settings), nil
}
