package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
)

// SensitiveCommand is deliberately shared by public dispatch and private
// preparation. Automation executes internal methods under its already durable,
// reviewed budget; no caller-controlled skip flag crosses the public boundary.
func SensitiveCommand(method string) bool {
	switch method {
	case "wallet.send", "transaction.bump", "offer.create", "swap.take", "trade.confirm", "offer.cancel", "automation.save", "automation.disable", "strategy.save", "strategy.stop", "wallet.recovery", "wallet.backup", "pause":
		return true
	default:
		return false
	}
}

// ActionDigest canonicalizes JSON without float conversion. Monetary integers
// above 2^53 remain exact, and irrelevant object-key ordering is immaterial.
func ActionDigest(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", errors.New("invalid action payload")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return "", errors.New("invalid action payload")
	}
	return authorization.Digest(value)
}
func (e *Engine) authorizationAction(req Request) (authorization.Action, error) {
	if !SensitiveCommand(req.Method) {
		return authorization.Action{}, errors.New("action does not require sensitive consent")
	}
	if e.authorizationEpoch == "" {
		e.authorizationEpoch = transport.RandomID()
	}
	digest, err := ActionDigest(req.Params)
	if err != nil {
		return authorization.Action{}, err
	}
	return authorization.Action{Installation: e.Config.Installation, Wallet: e.Config.Name, WalletKey: e.identity.Public().Hex(), Network: string(e.Config.Network.Normalized()), Epoch: e.authorizationEpoch, Method: req.Method, Digest: digest}, nil
}

// AuthorizationAction captures current public context only. The OS prompt is
// performed by the native owner after this brief lock has been released.
func (e *Engine) AuthorizationAction(req Request) (authorization.Action, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fatal != nil {
		return authorization.Action{}, e.fatal
	}
	return e.authorizationAction(req)
}
func (e *Engine) authorizeCommand(ctx context.Context, req Request) error {
	if !SensitiveCommand(req.Method) {
		return nil
	}
	if e.Config.CredentialMode != "native" && e.Config.Authorization == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// These exact duplicate reads cannot sign or add authority. Saved signed
	// sends/accepted receipts continue through Tick without a new consent prompt.
	if req.Method == "wallet.send" {
		var p SendRequest
		if json.Unmarshal(req.Params, &p) == nil {
			if s := e.s.Sends[p.ID]; s != nil && s.Raw != "" && s.Digest == protocol.Digest(p) {
				return nil
			}
		}
	}
	if req.Method == "trade.confirm" {
		var p ConfirmTradeRequest
		if json.Unmarshal(req.Params, &p) == nil {
			if r := e.s.TradeReceipts[p.RequestID]; r != nil && r.Result.State != "pending" && r.Digest == protocol.Digest(p) {
				return nil
			}
		}
	}
	if e.Config.Authorization == nil {
		return authorization.ErrRequired
	}
	action, err := e.authorizationAction(req)
	if err != nil {
		return err
	}
	return e.Config.Authorization.Consume(ctx, action)
}
