package desktop

import (
	"context"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/chain"
)

func (m *Manager) inspectPortable(ctx context.Context, request *pb.InspectBackupRequest) (*pb.BackupContents, error) {
	manifest, legacy, err := readBackupManifest(ctx, m.root, request.Path, request.Password)
	if err != nil {
		return nil, err
	}
	defer manifest.close()
	result := &pb.BackupContents{FormatVersion: int32(manifest.FormatVersion), CreatedAt: manifest.CreatedAt, Legacy: legacy, Warning: "Import creates a new isolated wallet profile. Recovery checks current chain evidence before allowing new trades. Old orders stay quarantined; an older file can omit later activity or secrets."}
	if legacy {
		result.Warning += " This legacy file has no reliable snapshot creation time."
		result.CreatedAt = 0
	}
	for _, wallet := range manifest.Wallets {
		entry := &pb.BackupWalletEntry{SourceWalletId: wallet.ID, Name: wallet.Name}
		for _, network := range []chain.Network{chain.Regtest, chain.Testnet, chain.Mainnet} {
			if _, exists := wallet.Networks[network]; exists {
				entry.Networks = append(entry.Networks, string(network))
			}
		}
		result.Wallets = append(result.Wallets, entry)
	}
	return result, nil
}
func (m *Manager) exportPortableAPI(ctx context.Context, profile string, request *pb.ExportPortableBackupRequest) (*pb.PortableBackupResult, error) {
	result, err := m.exportPortable(ctx, profile, request.Path, request.Password, request.AllWallets)
	if err != nil {
		return nil, err
	}
	return &pb.PortableBackupResult{Path: result.Path, CreatedAt: result.CreatedAt, Wallets: int32(result.Wallets), Networks: int32(result.Networks), ReminderWarning: result.ReminderWarning}, nil
}
func (m *Manager) importPortableAPI(ctx context.Context, request *pb.ImportBackupRequest) (*pb.ImportBackupResult, error) {
	result, err := m.importPortable(ctx, portableImportRequest{Path: request.Path, Password: request.Password, SourceWallet: request.SourceWalletId, Name: request.Name, Revision: request.Revision})
	if err != nil {
		return nil, err
	}
	return &pb.ImportBackupResult{ProfileId: result.ProfileID, Settings: result.Settings, Legacy: result.Legacy}, nil
}
