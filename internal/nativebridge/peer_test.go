package nativebridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/credential"
)

func pair(t *testing.T) (*Peer, *Peer) {
	t.Helper()
	a, b := net.Pipe()
	left, err := New(context.Background(), "owned session", a, a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := New(context.Background(), "owned session", b, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(left.Close)
	t.Cleanup(right.Close)
	return left, right
}
func TestPrivatePeerConcurrentRepliesAndClose(t *testing.T) {
	a, b := pair(t)
	b.Handle(func(ctx context.Context, method string, raw json.RawMessage) (any, string) {
		var v int
		if method != "echo" || json.Unmarshal(raw, &v) != nil {
			return nil, "invalid"
		}
		return v * 2, ""
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			var got int
			if err := a.Call(context.Background(), "echo", v, &got); err != nil || got != v*2 {
				t.Errorf("reply mismatch %v", err)
			}
		}(i)
	}
	wg.Wait()
	b.Close()
	var output int
	if err := a.Call(context.Background(), "echo", 2, &output); err == nil {
		t.Fatal("closed peer accepted request")
	}
}
func TestPrivatePeerRejectsSessionOversizeAndUnframedInput(t *testing.T) {
	for _, mode := range []string{"session", "oversize", "unframed"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			peer, err := New(context.Background(), "owner", a, a)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			defer b.Close()
			go func() {
				if mode == "unframed" {
					_, _ = b.Write([]byte("synthetic log output"))
					return
				}
				var raw []byte
				var size [4]byte
				if mode == "oversize" {
					binary.BigEndian.PutUint32(size[:], MaxFrame+1)
				} else {
					raw, _ = json.Marshal(frame{Version: 1, Session: "other", ID: "0000000000000000000000000000000000000000000000000000000000000000", Kind: "request", Method: "read"})
					binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
				}
				_, _ = b.Write(size[:])
				if raw != nil {
					_, _ = b.Write(raw)
				}
			}()
			select {
			case <-peer.Done():
			case <-time.After(time.Second):
				t.Fatal("malformed private framing retained connection")
			}
		})
	}
}
func TestPrivatePeerCancellationRevokesPendingAndCancelsHandler(t *testing.T) {
	a, b := pair(t)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	b.Handle(func(ctx context.Context, _ string, _ json.RawMessage) (any, string) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return nil, "cancelled"
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Call(ctx, "pending", nil, nil) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("handler survived cancellation")
	}
	a.mu.Lock()
	n := len(a.pending)
	a.mu.Unlock()
	if n != 0 {
		t.Fatal("cancelled reply retained permission slot")
	}
}
func TestPrivateStoreReturnsOwnedBytesAndSanitizesProviderErrors(t *testing.T) {
	a, b := pair(t)
	secret := []byte("isolated synthetic provider credential")
	mode := "get"
	b.Handle(func(_ context.Context, method string, raw json.RawMessage) (any, string) {
		var request credentialRequest
		if json.Unmarshal(raw, &request) != nil {
			return nil, "invalid"
		}
		if request.Key.Installation != "isolated" {
			return nil, "denied"
		}
		if mode != "get" {
			return nil, mode
		}
		if method != "credential.get" {
			return nil, "invalid"
		}
		return credentialReply{Password: secret}, ""
	})
	store := Store{Peer: a}
	key := credential.Key{Installation: "isolated", Profile: "test"}
	first, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	clear(first)
	second, err := store.Get(context.Background(), key)
	if err != nil || string(second) != string(secret) {
		t.Fatal("reader lifetime shared", err)
	}
	clear(second)
	for _, test := range []struct {
		code string
		want error
	}{{"denied", credential.ErrDenied}, {"locked", credential.ErrLocked}, {"missing", credential.ErrMissing}, {"unknown", credential.ErrUnavailable}} {
		// Previous Call completed before changing the injected reply mode.
		mode = test.code
		p, err := store.Get(context.Background(), key)
		if !errors.Is(err, test.want) || p != nil {
			clear(p)
			t.Fatal("native error selected credential", err)
		}
	}
}

type queuedReplyWriter struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (w *queuedReplyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.closed
	return 0, ErrClosed
}
func (w *queuedReplyWriter) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return nil
}
func TestPrivatePeerCancellationClearsAlreadyQueuedReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	writer := &queuedReplyWriter{started: make(chan struct{}), closed: make(chan struct{})}
	p := &Peer{ctx: lifetime, cancel: stop, session: "isolated", reader: io.NopCloser(strings.NewReader("")), writer: writer, pending: map[string]chan reply{}, incoming: map[string]context.CancelFunc{}, done: make(chan struct{})}
	defer p.Close()
	done := make(chan error, 1)
	go func() { var output any; done <- p.Call(ctx, "credential.get", nil, &output) }()
	<-writer.started
	raw := []byte(`{"password":"isolated queued reply bytes"}`)
	p.mu.Lock()
	for id, ch := range p.pending {
		delete(p.pending, id)
		ch <- reply{payload: raw}
		break
	}
	p.mu.Unlock()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, b := range raw {
		if b != 0 {
			t.Fatal("cancelled Call retained a queued provider reply")
		}
	}
}
