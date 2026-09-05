// Package api exposes the generated protobuf API over gRPC and grpc-gateway.
package api

import (
	"context"
	"encoding/json"
	"errors"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Service struct {
	pb.UnimplementedDaemonServiceServer
	Command       func(context.Context, daemon.Request) (any, error)
	ReadSettings  func(context.Context) (*pb.Settings, error)
	WriteSettings func(context.Context, *pb.Settings) (*pb.Settings, error)
}

func rpcError(err error) error {
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request timed out")
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}
func (s *Service) command(ctx context.Context, method string, in, out proto.Message) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if s.Command == nil {
		return status.Error(codes.Unavailable, "wallet is not connected")
	}
	result, err := s.Command(ctx, daemon.Request{Method: method, Params: raw})
	if err != nil {
		return rpcError(err)
	}
	if _, empty := out.(*emptypb.Empty); empty {
		return nil
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return status.Error(codes.Internal, "cannot encode daemon result")
	}
	if err = protojson.Unmarshal(raw, out); err != nil {
		return status.Error(codes.Internal, "daemon result does not match API schema: "+err.Error())
	}
	return nil
}
func (s *Service) GetStatus(ctx context.Context, in *emptypb.Empty) (*pb.Status, error) {
	out := &pb.Status{}
	err := s.command(ctx, "status", in, out)
	return out, err
}
func (s *Service) SetPaused(ctx context.Context, in *pb.SetPausedRequest) (*pb.Status, error) {
	out := &pb.Status{}
	err := s.command(ctx, "pause", in, out)
	return out, err
}
func (s *Service) CreateOffer(ctx context.Context, in *pb.CreateOfferRequest) (*pb.Offer, error) {
	out := &pb.Offer{}
	err := s.command(ctx, "offer.create", in, out)
	return out, err
}
func (s *Service) CancelOffer(ctx context.Context, in *pb.CancelOfferRequest) (*pb.Offer, error) {
	out := &pb.Offer{}
	err := s.command(ctx, "offer.cancel", in, out)
	return out, err
}
func (s *Service) TakeOffer(ctx context.Context, in *pb.TakeOfferRequest) (*pb.TakeOfferResponse, error) {
	out := &pb.TakeOfferResponse{}
	err := s.command(ctx, "swap.take", in, out)
	return out, err
}
func (s *Service) Mine(ctx context.Context, in *pb.MineRequest) (*emptypb.Empty, error) {
	out := &emptypb.Empty{}
	err := s.command(ctx, "regtest.mine", in, out)
	return out, err
}
func (s *Service) Faucet(ctx context.Context, in *pb.FaucetRequest) (*pb.FaucetResponse, error) {
	out := &pb.FaucetResponse{}
	err := s.command(ctx, "regtest.faucet", in, out)
	return out, err
}
func (s *Service) GetRecovery(ctx context.Context, in *emptypb.Empty) (*pb.Recovery, error) {
	out := &pb.Recovery{}
	err := s.command(ctx, "wallet.recovery", in, out)
	return out, err
}
func (s *Service) BackupWallet(ctx context.Context, in *emptypb.Empty) (*pb.Backup, error) {
	out := &pb.Backup{}
	err := s.command(ctx, "wallet.backup", in, out)
	return out, err
}
func (s *Service) GetSettings(ctx context.Context, _ *emptypb.Empty) (*pb.Settings, error) {
	if s.ReadSettings == nil {
		return nil, status.Error(codes.Unimplemented, "settings are managed by this daemon's configuration file")
	}
	v, err := s.ReadSettings(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	return v, nil
}
func (s *Service) UpdateSettings(ctx context.Context, in *pb.Settings) (*pb.Settings, error) {
	if s.WriteSettings == nil {
		return nil, status.Error(codes.Unimplemented, "settings are managed by this daemon's configuration file")
	}
	v, err := s.WriteSettings(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return v, nil
}
func (s *Service) CheckNode(ctx context.Context, in *pb.CheckNodeRequest) (*pb.CheckNodeResponse, error) {
	n, id := chain.Network(in.Network), chain.ID(in.Chain)
	if !n.Valid() || !id.Valid() || in.Node == nil {
		return nil, status.Error(codes.InvalidArgument, "network, chain and node are required")
	}
	var b chain.Backend
	var err error
	trust := "Consensus validated by the configured full node."
	switch in.Node.Kind {
	case "electrum":
		b, err = chain.NewElectrum(n, id, in.Node.Url, in.Node.CertificateSha256)
		trust = "Indexer operator is trusted for canonical chain, completeness and chain work. Transactions and merkle inclusion are checked locally."
	case "rpc":
		b, err = chain.NewFor(n, id, in.Node.Url, in.Node.Cookie)
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown backend")
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer b.Close()
	if err = b.Check(ctx); err != nil {
		return nil, rpcError(err)
	}
	h, err := b.Height(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.CheckNodeResponse{Height: h, Trust: trust}, nil
}
