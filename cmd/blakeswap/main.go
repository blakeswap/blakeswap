package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/desktop"
	"github.com/blakeswap/blakeswap/internal/nativebridge"
	"github.com/blakeswap/blakeswap/internal/relay"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: blakeswap daemon|relay|call")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "desktop":
		f := flag.NewFlagSet("desktop", flag.ExitOnError)
		root := f.String("data-dir", "", "application data directory")
		parent := f.Int("parent-pid", 0, "owning GUI process ID")
		credentialMode := f.String("credential-mode", "native", "native app credentials, or explicit operator-controlled file mode")
		_ = f.Parse(os.Args[2:])
		if *credentialMode == "file" {
			return desktop.Run(ctx, *root, *parent, desktop.RunOptions{CredentialMode: "file"})
		}
		if *credentialMode != "native" {
			return fmt.Errorf("unknown credential mode")
		}
		session := os.Getenv("BLAKESWAP_DESKTOP_SESSION")
		if *parent <= 0 || session == "" {
			return fmt.Errorf("native desktop requires its app-owned security connection; use --credential-mode=file for explicit headless operation")
		}
		for _, pipe := range []*os.File{os.Stdin, os.Stdout} {
			info, err := pipe.Stat()
			if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				return fmt.Errorf("native security requires inherited anonymous pipes")
			}
		}
		privateCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		peer, err := nativebridge.New(privateCtx, session, os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		defer peer.Close()
		// Broker loss retires new-action consent inside Manager; accepted
		// settlement remains live until explicit app shutdown or parent death.
		authority, err := authorization.New(session, nil)
		if err != nil {
			return err
		}
		defer authority.Close()
		return desktop.Run(privateCtx, *root, *parent, desktop.RunOptions{CredentialMode: "native", Store: nativebridge.Store{Peer: peer}, Broker: peer, Authority: authority})
	case "daemon":
		f := flag.NewFlagSet("daemon", flag.ExitOnError)
		path := f.String("config", "", "config JSON")
		_ = f.Parse(os.Args[2:])
		raw, err := os.ReadFile(*path)
		if err != nil {
			return err
		}
		var cfg daemon.Config
		if err = json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
		if cfg.CredentialMode != "file" {
			return fmt.Errorf("headless daemon configuration requires explicit credential_mode: file")
		}
		engine, err := daemon.Open(ctx, cfg)
		if err != nil {
			return err
		}
		defer engine.Close()
		log.Printf("%s %s ready: %s", cfg.Name, cfg.Mode, cfg.Socket)
		server, err := api.Listen(ctx, cfg.Socket, &api.Service{Command: engine.Command})
		if err != nil {
			return err
		}
		defer server.Close()
		err = engine.Run(ctx)
		if err == context.Canceled {
			return nil
		}
		return err
	case "relay":
		f := flag.NewFlagSet("relay", flag.ExitOnError)
		path := f.String("db", ".local/relay.db", "durable relay database")
		addr := f.String("listen", "127.0.0.1:7447", "loopback listener")
		_ = f.Parse(os.Args[2:])
		host, _, err := net.SplitHostPort(*addr)
		if err != nil || host != "127.0.0.1" {
			return fmt.Errorf("development relay must bind 127.0.0.1")
		}
		r, err := relay.Open(*path)
		if err != nil {
			return err
		}
		defer r.Close()
		server := &http.Server{Addr: *addr, Handler: r, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
		go func() { <-ctx.Done(); _ = server.Close() }()
		log.Printf("Nostr relay listening on ws://%s", *addr)
		err = server.ListenAndServe()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case "call":
		f := flag.NewFlagSet("call", flag.ExitOnError)
		socket := f.String("socket", "", "daemon socket")
		method := f.String("method", "status", "local method")
		params := f.String("params", "{}", "JSON parameters")
		_ = f.Parse(os.Args[2:])
		result, err := api.Call(ctx, *socket, daemon.Request{Method: *method, Params: json.RawMessage(*params)})
		if err != nil {
			return err
		}
		fmt.Println(string(result))
		return nil
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}
