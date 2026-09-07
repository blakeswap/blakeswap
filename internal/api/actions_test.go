package api

import (
	"context"
	"encoding/json"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"os"
	"path/filepath"
	"testing"
)

func TestActionSummaryAuthenticatedTypedAllWallets(t *testing.T) {
	dir, err := os.MkdirTemp("", "bs-actions-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	service := &Service{Command: func(_ context.Context, r daemon.Request) (any, error) {
		var p struct {
			Refresh bool `json:"refresh"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil || r.Method != "actions.summary" || !p.Refresh {
			t.Fatal(r, err)
		}
		return daemon.ActionSummary{Network: chain.Mainnet, SettingsRevision: 9007199254740993, ObservedAt: 1800000000, Complete: false, RequiresMonitoring: true, Wallets: []daemon.WalletActions{{WalletID: "other-wallet", Network: chain.Mainnet, Known: true, Source: "live", Actions: []daemon.WalletAction{{ID: "swap/id", Kind: "swap", ObjectID: "id", State: "owner_claim", RequiresMonitoring: true, FirstReveal: true, TowerReady: true, Deadlines: []daemon.ActionDeadline{{Kind: "reveal", Chain: chain.BTC, Unit: "median_time", Target: 1800000100, Observed: 1800000000, ObservedAt: 1800000010, Remaining: 100, Certain: true, Band: "approaching"}}}}}, {WalletID: "opening", Network: chain.Mainnet}}}, nil
	}}
	server, err := Listen(context.Background(), filepath.Join(dir, "rpc.sock"), service)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := grpc.NewClient("unix://"+server.Endpoint.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewDaemonServiceClient(conn)
	if _, err := client.GetActionSummary(context.Background(), &pb.ActionSummaryRequest{Refresh: true}); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+server.Endpoint.Token)
	result, err := client.GetActionSummary(ctx, &pb.ActionSummaryRequest{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.SettingsRevision != 9007199254740993 || len(result.Wallets) != 2 || result.Complete || !result.RequiresMonitoring {
		t.Fatal(result)
	}
	a := result.Wallets[0].Actions[0]
	d := a.Deadlines[0]
	if !a.FirstReveal || !a.TowerReady || d.Unit != "median_time" || d.Remaining != 100 || !d.Certain || d.ObservedAt != 1800000010 || result.Wallets[1].Known {
		t.Fatal(result)
	}
}
