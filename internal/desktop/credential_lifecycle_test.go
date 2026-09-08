package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/nativebridge"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

func installedNativeManager(t *testing.T) (*Manager, *isolatedCredentialStore) {
	t.Helper()
	m, store := nativeManager(t)
	r := &pb.PrepareFirstWalletRequest{Name: "Existing native wallet", Revision: m.settings.Revision}
	if _, err := m.prepareFirstWallet(nativeConsent(t, m, "onboarding.prepare", r), r); err != nil {
		t.Fatal(err)
	}
	m.settings.OnboardingStage = ""
	if err := saveSettings(m.root, m.settings); err != nil {
		t.Fatal(err)
	}
	m.publishView()
	return m, store
}

func TestNativeAdditionalWalletFailureKeepsExistingStartup(t *testing.T) {
	for _, stage := range []string{"denied", "discovered", "created", "stored", "verified", "active", "cancelled-active"} {
		t.Run(stage, func(t *testing.T) {
			m, store := installedNativeManager(t)
			r := &pb.CreateWalletRequest{Name: "Additional wallet", Revision: m.settings.Revision}
			ctx, cancel := context.WithCancel(nativeConsent(t, m, "wallet.create", r))
			defer cancel()
			injected := errors.New("isolated installation interruption")
			if stage == "denied" {
				store.err = credential.ErrDenied
			} else {
				m.credentials.profiles.After = func(at string) error {
					if stage == "cancelled-active" && at == "active" {
						cancel()
						return nil
					}
					if stage == at {
						return injected
					}
					return nil
				}
			}
			if _, err := m.createWallet(ctx, r); err == nil {
				t.Fatal("interruption ignored")
			}
			store.err = nil
			if len(m.settings.Wallets) != 1 || m.unpublishedInstalls.Load() != 0 || m.installations.Load() != 0 {
				t.Fatal("failed private installation published")
			}
			c, err := openProfileCredentials(context.Background(), m.root, store)
			if err != nil {
				t.Fatal("failed private creation blocked existing startup", err)
			}
			saved, err := loadSettingsWithReader(m.root, c.readMaster)
			if err != nil || len(saved.Wallets) != 1 {
				t.Fatal("failed private creation changed saved wallets", err)
			}
			if len(store.values) != 1 {
				t.Fatal("failed unpublished creation retained its new item")
			}
		})
	}
}

func TestNativeAbandonedPrivateCreationDoesNotBecomePublished(t *testing.T) {
	m, store := installedNativeManager(t)
	staging, err := os.MkdirTemp(filepath.Join(m.root, "wallets"), ".new-")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	m.credentials.profiles.After = func(at string) error {
		if at == "active" {
			return errors.New("simulated process interruption before publication")
		}
		return nil
	}
	// Deliberately retain staging and its item, as process death skips defers.
	if err = m.initializeMaster(context.Background(), staging, "wallet-0123456789abcdef", seed); err == nil {
		t.Fatal("interruption missed")
	}
	if len(store.values) != 2 {
		t.Fatal("test did not retain ambiguous private creation")
	}
	c, err := openProfileCredentials(context.Background(), m.root, store)
	if err != nil {
		t.Fatal("abandoned private directory blocked startup", err)
	}
	saved, err := loadSettingsWithReader(m.root, c.readMaster)
	if err != nil || len(saved.Wallets) != 1 {
		t.Fatal("unpublished identity installed", err)
	}
	m.removeStaging(staging, "wallet-0123456789abcdef")
	if len(store.values) != 1 {
		t.Fatal("private cleanup affected wrong credential")
	}
}

func TestNativePublishedAdditionalWalletSurvivesSettingsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "settings-failure"}[fail], func(t *testing.T) {
			m, store := installedNativeManager(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The short socket path is required on macOS.
			dir, err := os.MkdirTemp("", "bs-create-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			m.runtimeCtx, m.runtimeDir, m.servers = ctx, dir, make(map[string]*api.Server)
			defer func() {
				for _, s := range m.servers {
					s.Close()
				}
			}()
			path := filepath.Join(m.root, "settings.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			r := &pb.CreateWalletRequest{Name: "Additional verified profile", Revision: m.settings.Revision}
			_, err = m.createWallet(nativeConsent(t, m, "wallet.create", r), r)
			if (err != nil) != fail {
				t.Fatal("unexpected publication result", err)
			}
			if len(store.values) != 2 {
				t.Fatal("published credential deleted")
			}
			if fail {
				if m.unpublishedInstalls.Load() != 1 {
					t.Fatal("unpublished durable profile lost shutdown hold")
				}
				summary, err := m.actionSummary(context.Background(), daemon.Request{Params: json.RawMessage("{}")})
				if err != nil || !summary.InstallationPending || !summary.RequiresMonitoring || summary.Complete {
					t.Fatal("failed publication reported known empty", err)
				}
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else if m.unpublishedInstalls.Load() != 0 {
				t.Fatal("successful creation left installation hold")
			}
			c, err := openProfileCredentials(context.Background(), m.root, store)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				entries, err := os.ReadDir(filepath.Join(m.root, "wallets"))
				if err != nil {
					t.Fatal(err)
				}
				var markerPath string
				for _, entry := range entries {
					if walletID.MatchString(entry.Name()) && entry.Name() != "alice" {
						markerPath = filepath.Join(m.root, "wallets", entry.Name(), "profile.json")
					}
				}
				marker, err := os.ReadFile(markerPath)
				if err != nil {
					t.Fatal(err)
				}
				for _, invalid := range []string{"identity", "permission", "size"} {
					switch invalid {
					case "identity":
						if err = os.WriteFile(markerPath, []byte("{\"name\":\"Additional verified profile\",\"identity\":\"wrong\"}"), 0600); err != nil {
							t.Fatal(err)
						}
					case "permission":
						if err = os.Chmod(markerPath, 0644); err != nil {
							t.Fatal(err)
						}
					case "size":
						if err = os.WriteFile(markerPath, make([]byte, 4097), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := loadSettingsWithReader(m.root, c.readMaster); err == nil {
						t.Fatal("invalid interrupted profile accepted", invalid)
					}
					if len(store.values) != 2 {
						t.Fatal("invalid marker deleted a published credential")
					}
					if err = os.WriteFile(markerPath, marker, 0600); err != nil {
						t.Fatal(err)
					}
					if err = os.Chmod(markerPath, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			saved, err := loadSettingsWithReader(m.root, c.readMaster)
			if err != nil || len(saved.Wallets) != 2 {
				t.Fatal("restart lost verified published profile", err)
			}
			for _, profile := range saved.Wallets {
				noPasswordFile(t, filepath.Join(m.root, "wallets", profile.Id))
			}
			again, err := loadSettingsWithReader(m.root, c.readMaster)
			if err != nil || again.Revision != saved.Revision || len(again.Wallets) != 2 {
				t.Fatal("restart duplicated profile", err)
			}
		})
	}
}

func TestNativePrivatePeerDoneImmediatelyRevokesPublicGrant(t *testing.T) {
	m, server := nativeAPIFixture(t)
	r := &pb.PrepareFirstWalletRequest{Name: "Existing native wallet", Revision: m.settings.Revision}
	if _, err := m.prepareFirstWallet(nativeConsent(t, m, "onboarding.prepare", r), r); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	helper, err := nativebridge.New(context.Background(), "isolated broker session", a, a)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	owner, err := nativebridge.New(context.Background(), "isolated broker session", b, b)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	m.attachBroker(helper)
	var challenge authorization.Challenge
	if err := owner.Call(context.Background(), "consent.prepare", consentRequest{Profile: "alice", Method: "onboarding.get", Params: json.RawMessage("{}")}, &challenge); err != nil {
		t.Fatal(err)
	}
	var approved bool
	if err := owner.Call(context.Background(), "consent.approve", challenge, &approved); err != nil || !approved {
		t.Fatal("approve", err)
	}
	conn, err := grpc.NewClient("unix://"+server.Endpoint.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewDaemonServiceClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+server.Endpoint.Token, "x-blakeswap-consent", challenge.ID)
	owner.Close()
	select {
	case <-helper.Done():
	case <-time.After(time.Second):
		t.Fatal("helper peer not closed")
	}
	// No grace period: peer completion itself ends consent validity.
	if _, err := client.GetFirstWallet(ctx, &emptypb.Empty{}); err == nil {
		t.Fatal("closed private owner left public recovery grant usable")
	}
	if err := m.authority.Approve(challenge); err == nil {
		t.Fatal("late approval revived closed session")
	}
	if m.stopped {
		t.Fatal("consent teardown stopped settlement manager")
	}
}
