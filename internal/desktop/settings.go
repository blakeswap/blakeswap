package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"fiatjaf.com/nostr/nip19"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"google.golang.org/protobuf/encoding/protojson"
)

func Defaults() *pb.Settings {
	userDir, _ := os.UserHomeDir()
	relays := []string{"wss://nos.lol", "wss://relay.primal.net", "wss://relay.ditto.pub"}
	return &pb.Settings{ActiveNetwork: "mainnet", Revision: 1, OnboardingStage: "wallet", Wallets: []*pb.WalletProfile{{Id: "alice", Name: "Wallet 1"}}, Environments: []*pb.Environment{
		{Network: "regtest", Nodes: map[string]*pb.Node{"btc": {Kind: "rpc", Url: "http://127.0.0.1:19443", Cookie: filepath.Join(userDir, "Library/Application Support/Bitcoin/regtest/.cookie")}, "blake": {Kind: "rpc", Url: "http://127.0.0.1:29443", Cookie: filepath.Join(userDir, "Library/Application Support/BitcoinBlake2b/regtest/.cookie")}}, Relays: append([]string(nil), relays...), Tower: &pb.Tower{}},
		{Network: "testnet", Nodes: map[string]*pb.Node{"btc": {Kind: "electrum", Url: "ssl://mempool.space:40002"}, "blake": {Kind: "electrum"}}, Relays: relays, Tower: &pb.Tower{}},
		{Network: "mainnet", Nodes: map[string]*pb.Node{"btc": {Kind: "electrum", Url: "ssl://electrum.blockstream.info:50002"}, "blake": {Kind: "electrum", Url: "ssl://fulcrum.kilombino.com:17717", CertificateSha256: "506dadc710c5abaeb13191056c5aaf47035d30e08bd869f7b4fbe6e13745d5a7"}}, Relays: append([]string(nil), relays...), Tower: &pb.Tower{}},
	}}
}
func environment(s *pb.Settings, network string) *pb.Environment {
	for _, env := range s.Environments {
		if env.Network == network {
			return env
		}
	}
	return nil
}
func validate(s *pb.Settings) error {
	if s == nil || !chain.Network(s.ActiveNetwork).Valid() || s.ActiveNetwork == "" || len(s.Environments) != 3 {
		return errors.New("settings require all three networks and an active network")
	}
	if len(s.Wallets) == 0 || len(s.Wallets) > 20 {
		return errors.New("configure between one and 20 wallets")
	}
	if s.OnboardingStage != "" && s.OnboardingStage != "wallet" && s.OnboardingStage != "backup" && s.OnboardingStage != "connect" {
		return errors.New("invalid onboarding stage")
	}
	if s.OnboardingStage != "" && (len(s.Wallets) != 1 || s.Wallets[0] == nil || s.Wallets[0].Id != "alice") {
		return errors.New("onboarding requires the first wallet slot")
	}
	ids := map[string]bool{}
	for _, profile := range s.Wallets {
		if profile == nil || !walletID.MatchString(profile.Id) || ids[profile.Id] {
			return errors.New("invalid or duplicate wallet ID")
		}
		if err := validateWalletName(profile.Name); err != nil {
			return err
		}
		ids[profile.Id] = true
	}
	seen := map[string]bool{}
	for _, env := range s.Environments {
		if env == nil || env.Network == "" || !chain.Network(env.Network).Valid() || seen[env.Network] {
			return errors.New("invalid or duplicate environment")
		}
		seen[env.Network] = true
		if len(env.Nodes) != 2 || env.Nodes["btc"] == nil || env.Nodes["blake"] == nil {
			return errors.New("both node settings are required")
		}
		for id, cfg := range env.Nodes {
			// An empty endpoint is an explicit unconfigured environment. It cannot trade.
			if cfg.Url == "" {
				if cfg.Kind != "electrum" && cfg.Kind != "rpc" {
					return errors.New("unknown node backend")
				}
				continue
			}
			var b chain.Backend
			var err error
			switch cfg.Kind {
			case "electrum":
				b, err = chain.NewElectrum(chain.Network(env.Network), chain.ID(id), cfg.Url, cfg.CertificateSha256)
			case "rpc":
				if !filepath.IsAbs(cfg.Cookie) {
					return errors.New("RPC cookie path must be absolute")
				}
				b, err = chain.NewFor(chain.Network(env.Network), chain.ID(id), cfg.Url, cfg.Cookie)
			default:
				err = errors.New("unknown node backend")
			}
			if err != nil {
				return fmt.Errorf("%s %s: %w", env.Network, id, err)
			}
			b.Close()
		}
		if len(env.Relays) < 1 || len(env.Relays) > 3 {
			return errors.New("configure one to three relays per environment")
		}
		urls := map[string]bool{}
		for _, endpoint := range env.Relays {
			u, err := url.Parse(endpoint)
			if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || urls[endpoint] {
				return errors.New("invalid or duplicate relay URL")
			}
			urls[endpoint] = true
			if u.Scheme != "wss" && (u.Scheme != "ws" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1")) {
				return errors.New("relays require WSS or loopback WS")
			}
		}
		if len(env.FavoriteWatchtowers) > 100 {
			return errors.New("at most 100 favorite watchtowers per network")
		}
		favorites := map[string]bool{}
		for i, value := range env.FavoriteWatchtowers {
			pub, err := protocol.PublicKey(value)
			if err != nil {
				return err
			}
			npub := nip19.EncodeNpub(pub)
			if favorites[npub] {
				return errors.New("duplicate favorite watchtower")
			}
			favorites[npub] = true
			env.FavoriteWatchtowers[i] = npub
		}
		if env.Tower != nil && (env.Tower.Bps < 0 || env.Tower.Bps > 1000) {
			return errors.New("invalid tower rate")
		}
		if env.RescueFeeBps < 0 || env.RescueFeeBps > 1000 {
			return errors.New("rescue fee must be 1–1000 basis points (0 uses the 50 basis-point default)")
		}
	}
	return nil
}
func loadSettings(root string) (*pb.Settings, error) {
	path := filepath.Join(root, "settings.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s := Defaults()
		return s, saveSettings(root, s)
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<10 {
		return nil, errors.New("settings file too large")
	}
	s := &pb.Settings{}
	if err = protojson.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	// Legacy profiles used these stable directories; never generate replacement seeds.
	migrated := len(s.Wallets) == 0
	if migrated {
		s.Wallets = []*pb.WalletProfile{{Id: "alice", Name: "Alice"}}
		if _, err := os.Stat(filepath.Join(root, "wallets", "bob")); err == nil {
			s.Wallets = append(s.Wallets, &pb.WalletProfile{Id: "bob", Name: "Bob"})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		s.Revision++
	}
	if err := validate(s); err != nil {
		return nil, err
	}
	if migrated {
		return s, saveSettings(root, s)
	}
	if s.OnboardingStage == "wallet" {
		if err := recoverPreparedWallet(root, s); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func saveSettings(root string, s *pb.Settings) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(root, "settings.json"), raw)
}
func writePrivate(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

var walletID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validateWalletName(name string) error {
	if name != strings.TrimSpace(name) || name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 40 {
		return errors.New("wallet name must contain 1–40 characters without surrounding spaces")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("wallet name cannot contain control characters")
		}
	}
	return nil
}
