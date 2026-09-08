// Package authorization holds ephemeral, exact-action consent. Only the owned
// native broker may prepare or approve grants; public transports may consume
// one but cannot create permission. No grant belongs in durable wallet state.
package authorization

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var (
	ErrRequired = errors.New("authenticate this exact action in the native app")
	ErrChanged  = errors.New("authorization expired, was revoked, or belongs to different reviewed terms")
	ErrCapacity = errors.New("too many pending authentication requests; finish or cancel one")
)

const Lifetime = 2 * time.Minute
const MaxPending = 32

// Action includes the normalized typed payload digest and the current engine or
// settings epoch, in addition to public wallet identity and network. Epochs are
// replaced on helper/engine or relevant settings changes.
type Action struct {
	Installation string `json:"installation"`
	Wallet       string `json:"wallet"`
	WalletKey    string `json:"wallet_key"`
	Network      string `json:"network"`
	Epoch        string `json:"epoch"`
	Method       string `json:"method"`
	Digest       string `json:"digest"`
}

func (a Action) valid() bool {
	return a.Installation != "" && len(a.Installation) <= 128 && a.Wallet != "" && len(a.Wallet) <= 128 && len(a.WalletKey) <= 128 && len(a.Network) <= 32 && a.Epoch != "" && len(a.Epoch) <= 128 && a.Method != "" && len(a.Method) <= 128 && len(a.Digest) == 64
}

func Digest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

type Challenge struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Action  Action `json:"action"`
	Expires int64  `json:"expires"`
}

type pending struct {
	challenge Challenge
	expires   time.Time
	approved  bool
}

type Authority struct {
	mu       sync.Mutex
	session  string
	now      func() time.Time
	pending  map[string]pending
	closed   bool
	lifetime <-chan struct{}
}

// BindLifetime permanently binds this authority to one owned connection. A
// completed connection is checked synchronously by every permission operation,
// so scheduler delay in a cleanup watcher cannot extend a previously approved
// grant. A replacement helper/connection needs a new Authority.
func (s *Authority) BindLifetime(done <-chan struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if done == nil || (s.lifetime != nil && s.lifetime != done) || s.closed {
		return ErrChanged
	}
	s.lifetime = done
	if s.unavailable() {
		return ErrChanged
	}
	return nil
}

func (s *Authority) unavailable() bool {
	if s.lifetime != nil {
		select {
		case <-s.lifetime:
			s.closed = true
			clear(s.pending)
		default:
		}
	}
	return s.closed
}

func New(session string, now func() time.Time) (*Authority, error) {
	if session == "" || len(session) > 128 {
		return nil, errors.New("invalid native authentication session")
	}
	if now == nil {
		now = time.Now
	}
	return &Authority{session: session, now: now, pending: map[string]pending{}}, nil
}

func (s *Authority) prune() {
	now := s.now()
	for id, p := range s.pending {
		if !now.Before(p.expires) {
			delete(s.pending, id)
		}
	}
}

// Prepare is a private broker operation, never a public RPC.
func (s *Authority) Prepare(action Action) (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if s.unavailable() || !action.valid() {
		return Challenge{}, ErrChanged
	}
	if len(s.pending) >= MaxPending {
		return Challenge{}, ErrCapacity
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Challenge{}, err
	}
	expires := s.now().Add(Lifetime)
	c := Challenge{ID: hex.EncodeToString(random[:]), Session: s.session, Action: action, Expires: expires.Unix()}
	s.pending[c.ID] = pending{challenge: c, expires: expires}
	return c, nil
}

// Approve requires the exact challenge returned to the native owner. A stale
// reply after cancellation, lock, context change or helper replacement fails.
func (s *Authority) Approve(challenge Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	p, ok := s.pending[challenge.ID]
	if s.unavailable() || !ok || p.approved || challenge != p.challenge {
		return ErrChanged
	}
	p.approved = true
	s.pending[challenge.ID] = p
	return nil
}

type contextKey struct{}

// WithGrant carries an opaque one-use identifier, not an assertion of consent.
// Only Consume can establish permission after checking private stored approval.
func WithGrant(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

func (s *Authority) Consume(ctx context.Context, action Action) error {
	id, _ := ctx.Value(contextKey{}).(string)
	if id == "" {
		return ErrRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	p, ok := s.pending[id]
	delete(s.pending, id) // Every attempted use is one-shot, including a mismatch.
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.unavailable() || !ok || !p.approved || action != p.challenge.Action || p.challenge.Session != s.session {
		return ErrChanged
	}
	return nil
}

func (s *Authority) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

// Revoke affects only new action consent. It has no reference to keys, saved
// transactions, trade receipts, policy limits or settlement workers.
func (s *Authority) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.pending)
}

func (s *Authority) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	clear(s.pending)
}
