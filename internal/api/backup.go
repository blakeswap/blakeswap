package api

import (
	"context"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) ExportPortableBackup(ctx context.Context, in *pb.ExportPortableBackupRequest) (*pb.PortableBackupResult, error) {
	if s.PortableExport == nil {
		return nil, status.Error(codes.Unimplemented, "portable backups are managed by the desktop app")
	}
	result, err := s.PortableExport(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return result, nil
}
func (s *Service) InspectBackup(ctx context.Context, in *pb.InspectBackupRequest) (*pb.BackupContents, error) {
	if s.BackupInspect == nil {
		return nil, status.Error(codes.Unimplemented, "backup imports are managed by the desktop app")
	}
	result, err := s.BackupInspect(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return result, nil
}
func (s *Service) ImportBackup(ctx context.Context, in *pb.ImportBackupRequest) (*pb.ImportBackupResult, error) {
	if s.BackupImport == nil {
		return nil, status.Error(codes.Unimplemented, "backup imports are managed by the desktop app")
	}
	result, err := s.BackupImport(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return result, nil
}
