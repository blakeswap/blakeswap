package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/nativebridge"
)

type profileContextKey struct{}

func withProfile(ctx context.Context, profile string) context.Context {
	return context.WithValue(ctx, profileContextKey{}, profile)
}
func directSensitive(method string) bool {
	switch method {
	case "settings.update", "wallet.create", "backup.export", "backup.import", "onboarding.prepare", "onboarding.get", "onboarding.confirm", "onboarding.export", "onboarding.finish":
		return true
	}
	return false
}
func (m *Manager) directActionLocked(profile, method string, raw json.RawMessage) (authorization.Action, error) {
	if m.credentials == nil || !directSensitive(method) {
		return authorization.Action{}, authorization.ErrRequired
	}
	if m.stopped {
		return authorization.Action{}, errors.New("daemon is stopping")
	}
	found := false
	for _, wallet := range m.settings.Wallets {
		if wallet.Id == profile {
			found = true
		}
	}
	if !found {
		return authorization.Action{}, errors.New("wallet profile is unavailable")
	}
	walletKey := "uninitialized"
	if m.settings.OnboardingStage != "wallet" || profile != "alice" {
		journal, err := credential.ReadJournal(filepath.Join(m.root, "wallets", profile))
		if err != nil {
			return authorization.Action{}, err
		}
		if journal.Key.Installation != m.credentials.installation || journal.Key.Profile != profile {
			return authorization.Action{}, credential.ErrConflict
		}
		walletKey = journal.Identity
	}
	digest, err := daemon.ActionDigest(raw)
	if err != nil {
		return authorization.Action{}, err
	}
	return authorization.Action{Installation: m.credentials.installation, Wallet: profile, WalletKey: walletKey, Network: m.settings.ActiveNetwork, Epoch: fmt.Sprintf("settings/%d", m.settings.Revision), Method: method, Digest: digest}, nil
}
func (m *Manager) consumeDirectLocked(ctx context.Context, method string, input any) error {
	if m.credentials == nil && m.authority == nil {
		return nil
	}
	if m.authority == nil {
		return authorization.ErrRequired
	}
	profile, _ := ctx.Value(profileContextKey{}).(string)
	if profile == "" {
		return authorization.ErrRequired
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	defer clear(raw)
	action, err := m.directActionLocked(profile, method, raw)
	if err != nil {
		return err
	}
	return m.authority.Consume(ctx, action)
}

type consentRequest struct {
	Profile string          `json:"profile"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (m *Manager) brokerHandler(ctx context.Context, method string, raw json.RawMessage) (any, string) {
	if err := ctx.Err(); err != nil {
		return nil, "cancelled"
	}
	if m.authority == nil {
		return nil, "unavailable"
	}
	switch method {
	case "consent.prepare":
		var request consentRequest
		if json.Unmarshal(raw, &request) != nil {
			return nil, "invalid"
		}
		params, err := api.NormalizeSensitiveAction(request.Method, request.Params)
		if err != nil {
			return nil, "invalid"
		}
		defer clear(params)
		m.mu.Lock()
		var action authorization.Action
		if directSensitive(request.Method) {
			action, err = m.directActionLocked(request.Profile, request.Method, params)
		} else {
			engine := m.engines[request.Profile]
			if engine == nil || m.restart || m.stopped {
				err = errors.New("wallet is connecting")
			} else {
				action, err = engine.AuthorizationAction(daemon.Request{Method: request.Method, Params: params})
			}
		}
		m.mu.Unlock()
		if err != nil {
			return nil, "changed"
		}
		if ctx.Err() != nil {
			return nil, "cancelled"
		}
		challenge, err := m.authority.Prepare(action)
		if err != nil {
			return nil, "changed"
		}
		return challenge, ""
	case "consent.approve":
		var challenge authorization.Challenge
		if json.Unmarshal(raw, &challenge) != nil {
			return nil, "invalid"
		}
		if err := m.authority.Approve(challenge); err != nil {
			return nil, "changed"
		}
		return true, ""
	case "consent.cancel":
		var request struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &request) != nil {
			return nil, "invalid"
		}
		m.authority.Cancel(request.ID)
		return true, ""
	case "consent.revoke":
		m.authority.Revoke()
		return true, ""
	default:
		return nil, "invalid"
	}
}
func (m *Manager) attachBroker(peer *nativebridge.Peer) {
	if peer != nil {
		if m.authority == nil || m.authority.BindLifetime(peer.Done()) != nil {
			if m.authority != nil {
				m.authority.Close()
			}
			return
		}
		peer.Handle(m.brokerHandler)
		go func() { <-peer.Done(); m.authority.Close() }()
	}
}
