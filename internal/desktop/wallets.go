package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Called with the manager lock held (or before the run loop starts). Socket
// names stay short enough for macOS regardless of the stable wallet ID.
func (m *Manager) startAPI(profile string) error {
	service := &api.Service{Context: func(ctx context.Context) context.Context { return withProfile(ctx, profile) }, PortableExport: func(ctx context.Context, r *pb.ExportPortableBackupRequest) (*pb.PortableBackupResult, error) {
		return m.exportPortableAPI(ctx, profile, r)
	}, BackupInspect: m.inspectPortable, BackupImport: m.importPortableAPI, Command: func(ctx context.Context, r daemon.Request) (any, error) { return m.command(ctx, profile, r) }, ReadSettings: m.readSettings, WriteSettings: m.writeSettings, NewWallet: m.createWallet, PrepareWallet: m.prepareFirstWallet, FirstWallet: m.firstWallet, ConfirmWallet: m.confirmFirstWallet, ExportWallet: m.exportFirstWallet, FinishSetup: m.finishOnboarding}
	server, err := api.Listen(m.runtimeCtx, filepath.Join(m.runtimeDir, fmt.Sprintf("%d.sock", len(m.servers))), service)
	if err != nil {
		return err
	}
	m.servers[profile] = server
	return nil
}

// The native owner uses the PID and launch nonce to distinguish its child from
// another helper that already holds this installation's lock. Neither value
// grants API authority; the private endpoint token remains required.
type runtimeEndpoint struct {
	api.Endpoint
	OwnerPID       int    `json:"owner_pid"`
	CredentialMode string `json:"credential_mode"`
	OwnerSession   string `json:"owner_session,omitempty"`
}

func (m *Manager) writeRuntime() error {
	endpoints := map[string]runtimeEndpoint{}
	for id, server := range m.servers {
		mode := "file"
		if m.credentials != nil {
			mode = "native"
		}
		endpoints[id] = runtimeEndpoint{CredentialMode: mode, Endpoint: server.Endpoint, OwnerPID: os.Getpid(), OwnerSession: m.runtimeSession}
	}
	raw, err := json.Marshal(endpoints)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(m.root, "runtime.json"), raw)
}

func (m *Manager) createWallet(ctx context.Context, request *pb.CreateWalletRequest) (*pb.Settings, error) {
	defer m.beginInstallation()()
	if err := validateWalletName(request.Name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeDirectLocked(ctx, "wallet.create", request); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.stopped {
		return nil, status.Error(codes.Unavailable, "daemon is stopping")
	}
	if request.Revision != m.settings.Revision {
		return nil, status.Error(codes.Aborted, "settings changed; reload before creating a wallet")
	}
	if m.settings.OnboardingStage != "" {
		return nil, status.Error(codes.FailedPrecondition, "finish setting up your first wallet")
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
	staging, err := os.MkdirTemp(walletRoot, ".new-")
	if err != nil {
		return nil, err
	}
	defer m.removeStaging(staging, id)
	seed, err := wallet.NewMnemonic()
	if err != nil {
		return nil, err
	}
	if err := m.initializeMaster(ctx, staging, id, seed); err != nil {
		return nil, err
	}
	identity, err := backupIdentity(seed)
	if err != nil {
		return nil, err
	}
	marker, err := json.Marshal(preparedProfile{Name: request.Name, Identity: identity})
	if err != nil {
		return nil, err
	}
	if err = writePrivate(filepath.Join(staging, "profile.json"), marker); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	walletDir := filepath.Join(walletRoot, id)
	if _, err := os.Lstat(walletDir); !os.IsNotExist(err) {
		return nil, fmt.Errorf("generated wallet destination already exists")
	}
	if err = os.Rename(staging, walletDir); err != nil {
		return nil, err
	}
	m.unpublishedInstalls.Add(1)
	if err = syncDirectory(walletRoot); err != nil {
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
	m.unpublishedInstalls.Add(-1)
	return proto.Clone(saved).(*pb.Settings), nil
}
