package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/blakeswap/blakeswap/internal/daemon"
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
		engine, err := daemon.Open(ctx, cfg)
		if err != nil {
			return err
		}
		defer engine.Close()
		log.Printf("%s %s ready: %s", cfg.Name, cfg.Mode, cfg.Socket)
		errc := make(chan error, 2)
		go func() { errc <- engine.Serve(ctx) }()
		go func() { errc <- engine.Run(ctx) }()
		err = <-errc
		stop()
		<-errc
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
		result, err := daemon.Call(ctx, *socket, daemon.Request{Method: *method, Params: json.RawMessage(*params)})
		if err != nil {
			return err
		}
		fmt.Println(string(result))
		return nil
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}
