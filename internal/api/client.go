package api

import (
	"context"
	"encoding/json"
	"fmt"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Call preserves the development CLI vocabulary while using typed gRPC on wire.
func Call(ctx context.Context, socket string, req daemon.Request) (json.RawMessage, error) {
	ep, err := ReadEndpoint(socket + ".json")
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient("unix://"+ep.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("authorization", "Bearer "+ep.Token)
	ctx = metadata.NewOutgoingContext(ctx, md)
	client := pb.NewDaemonServiceClient(conn)
	var in proto.Message
	var invoke func() (proto.Message, error)
	switch req.Method {
	case "status":
		p := &emptypb.Empty{}
		in = p
		invoke = func() (proto.Message, error) { return client.GetStatus(ctx, p) }
	case "tower.resolve":
		p := &pb.ResolveWatchtowerRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.ResolveWatchtower(ctx, p) }
	case "pause":
		p := &pb.SetPausedRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.SetPaused(ctx, p) }
	case "offer.create":
		p := &pb.CreateOfferRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.CreateOffer(ctx, p) }
	case "offer.cancel":
		p := &pb.CancelOfferRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.CancelOffer(ctx, p) }
	case "swap.take":
		p := &pb.TakeOfferRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.TakeOffer(ctx, p) }
	case "regtest.mine":
		p := &pb.MineRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.Mine(ctx, p) }
	case "regtest.faucet":
		p := &pb.FaucetRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.Faucet(ctx, p) }
	case "wallet.recovery":
		p := &emptypb.Empty{}
		in = p
		invoke = func() (proto.Message, error) { return client.GetRecovery(ctx, p) }
	case "wallet.backup":
		p := &emptypb.Empty{}
		in = p
		invoke = func() (proto.Message, error) { return client.BackupWallet(ctx, p) }
	case "wallet.create":
		p := &pb.CreateWalletRequest{}
		in = p
		invoke = func() (proto.Message, error) { return client.CreateWallet(ctx, p) }
	case "settings.get":
		p := &emptypb.Empty{}
		in = p
		invoke = func() (proto.Message, error) { return client.GetSettings(ctx, p) }
	case "settings.update":
		p := &pb.Settings{}
		in = p
		invoke = func() (proto.Message, error) { return client.UpdateSettings(ctx, p) }
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
	if len(req.Params) > 0 {
		if err = protojson.Unmarshal(req.Params, in); err != nil {
			return nil, err
		}
	}
	// Bind a CLI action to the network observed before dispatch. Explicit callers
	// can supply expected_network to retain the context of an earlier screen/read.
	message := in.ProtoReflect()
	if field := message.Descriptor().Fields().ByName("expected_network"); field != nil && message.Get(field).String() == "" {
		current, err := client.GetStatus(ctx, &emptypb.Empty{})
		if err != nil {
			return nil, err
		}
		message.Set(field, protoreflect.ValueOfString(current.Network))
	}
	out, err := invoke()
	if err != nil {
		return nil, err
	}
	// CLI compatibility uses numeric satoshis. HTTP uses canonical protobuf JSON.
	return json.Marshal(out)
}
