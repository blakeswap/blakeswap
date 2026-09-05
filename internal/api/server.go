package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

//go:embed openapi.json
var OpenAPI []byte

type Endpoint struct {
	Socket string `json:"socket"`
	HTTP   string `json:"http"`
	Token  string `json:"token"`
}
type Server struct {
	Endpoint   Endpoint
	grpc       *grpc.Server
	http       *http.Server
	listener   net.Listener
	client     *grpc.ClientConn
	done       chan struct{}
	once       sync.Once
	credential string
}

func Listen(ctx context.Context, socket string, service *Service) (*Server, error) {
	if !filepath.IsAbs(socket) {
		return nil, errors.New("absolute Unix socket path required")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace a non-socket")
		}
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			conn.Close()
			return nil, errors.New("daemon is already running")
		}
		if err = os.Remove(socket); err != nil {
			return nil, err
		}
	}
	unix, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		unix.Close()
		return nil, err
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		unix.Close()
		return nil, err
	}
	var token [32]byte
	if _, err = rand.Read(token[:]); err != nil {
		unix.Close()
		tcp.Close()
		return nil, err
	}
	ep := Endpoint{Socket: socket, HTTP: "http://" + tcp.Addr().String(), Token: hex.EncodeToString(token[:])}
	server := &Server{Endpoint: ep, listener: unix, done: make(chan struct{}), credential: socket + ".json"}
	server.grpc = grpc.NewServer(grpc.MaxRecvMsgSize(128<<10), grpc.MaxSendMsgSize(8<<20), grpc.MaxConcurrentStreams(16), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		auth := md.Get("authorization")
		if len(auth) != 1 || !authorized(auth[0], ep.Token) {
			return nil, status.Error(codes.Unauthenticated, "invalid daemon credential")
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		return handler(ctx, req)
	}))
	pb.RegisterDaemonServiceServer(server.grpc, service)
	go func() { _ = server.grpc.Serve(unix) }()
	client, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20)))
	if err != nil {
		server.grpc.Stop()
		tcp.Close()
		return nil, err
	}
	server.client = client
	mux := runtime.NewServeMux(runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{MarshalOptions: protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}}), runtime.WithIncomingHeaderMatcher(func(key string) (string, bool) {
		if strings.EqualFold(key, "Authorization") {
			return "", false // grpc-gateway already forwards Authorization without a prefix.
		}
		return runtime.DefaultHeaderMatcher(key)
	}))
	if err = pb.RegisterDaemonServiceHandler(ctx, mux, client); err != nil {
		server.grpc.Stop()
		client.Close()
		tcp.Close()
		return nil, err
	}
	host := tcp.Addr().String()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != host || r.Header.Get("Origin") != "" {
			http.Error(w, "untrusted origin or host", http.StatusForbidden)
			return
		}
		if !authorized(r.Header.Get("Authorization"), ep.Token) {
			http.Error(w, "invalid daemon credential", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		if r.URL.Path == "/openapi.json" {
			if r.Method != "GET" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(OpenAPI)
			return
		}
		mux.ServeHTTP(w, r)
	})
	server.http = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 50 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	if err = writeEndpoint(server.credential, ep); err != nil {
		server.grpc.Stop()
		client.Close()
		tcp.Close()
		return nil, err
	}
	go func() { _ = server.http.Serve(tcp) }()
	go func() {
		select {
		case <-ctx.Done():
			server.Close()
		case <-server.done:
		}
	}()
	return server, nil
}
func authorized(value, token string) bool {
	return subtle.ConstantTimeCompare([]byte(value), []byte("Bearer "+token)) == 1
}
func writeEndpoint(path string, ep Endpoint) error {
	raw, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".runtime-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.http.Close()
		s.grpc.Stop()
		_ = s.client.Close()
		_ = s.listener.Close()
		_ = os.Remove(s.credential)
	})
}
func ReadEndpoint(path string) (Endpoint, error) {
	var ep Endpoint
	info, err := os.Lstat(path)
	if err != nil {
		return ep, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return ep, errors.New("runtime credentials must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ep, err
	}
	if len(raw) > 8192 {
		return ep, errors.New("runtime file too large")
	}
	if err = json.Unmarshal(raw, &ep); err != nil {
		return ep, err
	}
	if len(ep.Token) != 64 || !filepath.IsAbs(ep.Socket) {
		return ep, errors.New("invalid runtime credential file")
	}
	return ep, nil
}
