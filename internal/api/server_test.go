package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestGRPCAndGatewayAuthenticationAndExactIntegers(t *testing.T) {
	dir, err := os.MkdirTemp("", "bs-api-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var calls atomic.Int32
	service := &Service{Command: func(ctx context.Context, r daemon.Request) (any, error) {
		calls.Add(1)
		switch r.Method {
		case "status":
			return daemon.Status{Name: "alice", Network: chain.Regtest, Balances: map[chain.ID]int64{chain.BTC: 9007199254740993}, Funds: map[chain.ID]daemon.ChainBalance{chain.BTC: {TotalConfirmed: 9007199254740993, UnlockedConfirmed: 9007199254740990, ReservedConfirmed: 3, Unconfirmed: 7, HTLCLocked: 100000, HTLCAvailable: true}}, Coins: []daemon.PublicCoin{{Chain: chain.BTC, TxID: "coin", Amount: 3, Reserved: true, Holds: []daemon.CoinHold{{Kind: "offer", ID: "order", Reason: "Open order", Cancellable: true}}}}, Swaps: []daemon.PublicSwap{{ID: "test", Long: contract.HTLC{Chain: chain.BTC, Amount: 2000000, RefundHeight: 1800000000, TxID: "funding", Vout: 2}}}}, nil
		case "offer.create":
			var p struct {
				SellAmount int64 `json:"sell_amount"`
			}
			if err := json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"sell_amount": p.SellAmount}, nil
		default:
			t.Fatalf("unexpected method %s", r.Method)
			return nil, nil
		}
	}}
	server, err := Listen(context.Background(), filepath.Join(dir, "rpc.sock"), service)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, path := range []string{server.Endpoint.Socket, server.Endpoint.Socket + ".json"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("permissions %s: %v", path, err)
		}
	}
	conn, err := grpc.NewClient("unix://"+server.Endpoint.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewDaemonServiceClient(conn)
	if _, err = client.GetStatus(context.Background(), &emptypb.Empty{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated gRPC: %v", err)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+server.Endpoint.Token)
	result, err := client.GetStatus(ctx, &emptypb.Empty{})
	if err != nil || result.Balances["btc"] != 9007199254740993 {
		t.Fatal(result, err)
	}
	if funds := result.Funds["btc"]; funds == nil || funds.UnlockedConfirmed != 9007199254740990 || funds.ReservedConfirmed != 3 || !funds.HtlcAvailable || len(result.Coins) != 1 || len(result.Coins[0].Holds) != 1 || !result.Coins[0].Holds[0].Cancellable {
		t.Fatal("funds API mapping", result)
	}
	if len(result.Swaps) != 1 || result.Swaps[0].Long.Amount != 2000000 || result.Swaps[0].Long.RefundLocktime != 1800000000 || result.Swaps[0].Long.Txid != "funding" || result.Swaps[0].Long.Vout != 2 {
		t.Fatal("HTLC API mapping dropped fields", result.Swaps)
	}
	for _, test := range []struct {
		name, token, origin, host, path string
		want                            int
	}{
		{name: "missing auth", path: "/v1/status", want: 401},
		{name: "foreign origin", token: server.Endpoint.Token, origin: "https://evil.example", path: "/v1/status", want: 403},
		{name: "host rebinding", token: server.Endpoint.Token, host: "evil.example", path: "/v1/status", want: 403},
		{name: "status", token: server.Endpoint.Token, path: "/v1/status", want: 200},
		{name: "openapi", token: server.Endpoint.Token, path: "/openapi.json", want: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", server.Endpoint.HTTP+test.path, nil)
			if test.token != "" {
				r.Header.Set("Authorization", "Bearer "+test.token)
			}
			r.Header.Set("Origin", test.origin)
			if test.host != "" {
				r.Host = test.host
			}
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != test.want {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
			}
			if test.name == "status" {
				var out pb.Status
				if err = protojson.Unmarshal(raw, &out); err != nil || out.Balances["btc"] != 9007199254740993 {
					t.Fatal(string(raw), err)
				}
				if !bytes.Contains(raw, []byte(`"9007199254740993"`)) {
					t.Fatal("int64 not encoded as decimal string")
				}
			}
		})
	}
	if calls.Load() != 2 {
		t.Fatalf("rejected calls reached engine: %d", calls.Load())
	}
	request, _ := http.NewRequest("POST", server.Endpoint.HTTP+"/v1/offers", bytes.NewBufferString(`{"sellAmount":"9007199254740993"}`))
	request.Header.Set("Authorization", "Bearer "+server.Endpoint.Token)
	request.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(raw, []byte(`"9007199254740993"`)) {
		t.Fatal(string(raw))
	}
	legacy, err := Call(ctx, server.Endpoint.Socket, daemon.Request{Method: "status", Params: json.RawMessage(`{}`)})
	if err != nil || !bytes.Contains(legacy, []byte(`9007199254740993`)) {
		t.Fatal(string(legacy), err)
	}
	server.Close()
	if _, err = os.Stat(server.Endpoint.Socket + ".json"); !os.IsNotExist(err) {
		t.Fatal("credentials survived shutdown")
	}
}
