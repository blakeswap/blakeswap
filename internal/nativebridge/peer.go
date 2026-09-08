// Package nativebridge implements bounded private framing on the anonymous
// pipes inherited by the native app's own helper. It is never a network service.
package nativebridge

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

const MaxFrame = 128 << 10
const MaxPending = 32

var ErrClosed = errors.New("native security connection closed")
var ErrProtocol = errors.New("invalid native security message")
var ErrCapacity = errors.New("native security connection is busy")

type Handler func(context.Context, string, json.RawMessage) (any, string)
type frame struct {
	Version int             `json:"version"`
	Session string          `json:"session"`
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Method  string          `json:"method,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}
type reply struct {
	payload json.RawMessage
	code    string
}
type Peer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	session  string
	reader   io.ReadCloser
	writer   io.WriteCloser
	mu       sync.Mutex
	pending  map[string]chan reply
	incoming map[string]context.CancelFunc
	handler  Handler
	closed   bool
	writeMu  sync.Mutex
	done     chan struct{}
	once     sync.Once
}

func New(ctx context.Context, session string, reader io.ReadCloser, writer io.WriteCloser) (*Peer, error) {
	if session == "" || len(session) > 128 || reader == nil || writer == nil {
		return nil, ErrProtocol
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Peer{ctx: ctx, cancel: cancel, session: session, reader: reader, writer: writer, pending: map[string]chan reply{}, incoming: map[string]context.CancelFunc{}, done: make(chan struct{})}
	go p.read()
	go func() { <-ctx.Done(); p.Close() }()
	return p, nil
}
func (p *Peer) Handle(handler Handler) { p.mu.Lock(); defer p.mu.Unlock(); p.handler = handler }
func (p *Peer) Done() <-chan struct{}  { return p.done }
func (p *Peer) Close() {
	p.once.Do(func() {
		p.cancel()
		p.reader.Close()
		p.writer.Close()
		p.mu.Lock()
		p.closed = true
		for id, ch := range p.pending {
			delete(p.pending, id)
			close(ch)
		}
		for id, cancel := range p.incoming {
			delete(p.incoming, id)
			cancel()
		}
		p.mu.Unlock()
		close(p.done)
	})
}

// ErrorCode contains only a bounded protocol code, never a native/OS error
// description that could include request or credential material.
type ErrorCode string

func (e ErrorCode) Error() string { return "native security request failed: " + string(e) }
func validCode(code string) bool {
	if len(code) > 48 {
		return false
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r == '_') {
			return false
		}
	}
	return true
}
func (p *Peer) Call(ctx context.Context, method string, input, output any) error {
	if method == "" || len(method) > 64 {
		return ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return ErrProtocol
	}
	defer clear(payload)
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(random[:])
	ch := make(chan reply, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if len(p.pending) >= MaxPending {
		p.mu.Unlock()
		return ErrCapacity
	}
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		// Delivery also holds mu. Detach before draining so a response cannot
		// arrive just after cancellation leaves its owned credential bytes here.
		p.mu.Lock()
		delete(p.pending, id)
		select {
		case abandoned := <-ch:
			clear(abandoned.payload)
		default:
		}
		p.mu.Unlock()
	}()
	// A cancelled write closes this unusable connection to unblock the pipe. A
	// partial framed write cannot safely be resumed as another request.
	written := make(chan error, 1)
	go func() {
		written <- p.write(frame{Version: 1, Session: p.session, ID: id, Kind: "request", Method: method, Payload: payload})
	}()
	select {
	case err = <-written:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		p.Close()
		<-written
		return ctx.Err()
	}
	select {
	case response, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		defer clear(response.payload)
		if err = ctx.Err(); err != nil {
			return err
		}
		if response.code != "" {
			return ErrorCode(response.code)
		}
		if output != nil && json.Unmarshal(response.payload, output) != nil {
			return ErrProtocol
		}
		return nil
	case <-ctx.Done():
		// Revocation is local immediately; late responses are discarded by ID.
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		go func() { _ = p.write(frame{Version: 1, Session: p.session, ID: id, Kind: "cancel"}) }()
		return ctx.Err()
	}
}
func (p *Peer) write(message frame) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return ErrProtocol
	}
	defer clear(raw)
	if len(raw) == 0 || len(raw) > MaxFrame {
		return ErrProtocol
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.ctx.Done():
		return ErrClosed
	default:
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	if _, err = io.Copy(p.writer, bytesReader(size[:])); err == nil {
		_, err = io.Copy(p.writer, bytesReader(raw))
	}
	if err != nil {
		go p.Close()
		return ErrClosed
	}
	return nil
}

// This reader references the already-owned buffer; it adds no secret copy.
type byteReader struct{ data []byte }

func bytesReader(data []byte) *byteReader { return &byteReader{data: data} }
func (r *byteReader) Read(out []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(out, r.data)
	r.data = r.data[n:]
	return n, nil
}
func (p *Peer) read() {
	defer p.Close()
	for {
		var size [4]byte
		if _, err := io.ReadFull(p.reader, size[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(size[:])
		if n == 0 || n > MaxFrame {
			return
		}
		raw := make([]byte, n)
		if _, err := io.ReadFull(p.reader, raw); err != nil {
			clear(raw)
			return
		}
		var f frame
		err := json.Unmarshal(raw, &f)
		clear(raw)
		if err != nil || f.Version != 1 || f.Session != p.session || len(f.ID) != 64 || !validCode(f.Error) {
			clear(f.Payload)
			return
		}
		if _, err = hex.DecodeString(f.ID); err != nil {
			clear(f.Payload)
			return
		}
		p.mu.Lock()
		switch f.Kind {
		case "response":
			ch := p.pending[f.ID]
			delete(p.pending, f.ID)
			if ch != nil {
				ch <- reply{payload: f.Payload, code: f.Error}
			} else {
				clear(f.Payload)
			}
			p.mu.Unlock()
		case "cancel":
			if cancel := p.incoming[f.ID]; cancel != nil {
				cancel()
			}
			p.mu.Unlock()
			clear(f.Payload)
		case "request":
			handler := p.handler
			if f.Method == "" || len(f.Method) > 64 || p.incoming[f.ID] != nil || len(p.incoming) >= MaxPending {
				p.mu.Unlock()
				clear(f.Payload)
				return
			}
			ctx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
			p.incoming[f.ID] = cancel
			p.mu.Unlock()
			go p.dispatch(ctx, cancel, handler, f)
		default:
			p.mu.Unlock()
			clear(f.Payload)
			return
		}
	}
}
func (p *Peer) dispatch(ctx context.Context, cancel context.CancelFunc, handler Handler, f frame) {
	defer cancel()
	defer clear(f.Payload)
	defer func() { p.mu.Lock(); delete(p.incoming, f.ID); p.mu.Unlock() }()
	var output any
	code := "unavailable"
	if handler != nil {
		output, code = handler(ctx, f.Method, f.Payload)
	}
	if !validCode(code) {
		code = "failed"
	}
	if ctx.Err() != nil {
		code = "cancelled"
		output = nil
	}
	raw, err := json.Marshal(output)
	if err != nil {
		code = "failed"
		raw = nil
	}
	defer clear(raw)
	_ = p.write(frame{Version: 1, Session: p.session, ID: f.ID, Kind: "response", Payload: raw, Error: code})
}
