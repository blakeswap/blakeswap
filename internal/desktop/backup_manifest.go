package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/wallet"
)

// Everything in this manifest is inside the authenticated ciphertext. Source
// profile identifiers are descriptive only and never become filesystem paths.
type backupManifest struct {
	FormatVersion int            `json:"format_version"`
	CreatedAt     int64          `json:"created_at"`
	Wallets       []backupWallet `json:"wallets"`
}

type backupWallet struct {
	ID       string                          `json:"id"`
	Name     string                          `json:"name"`
	Identity string                          `json:"identity"`
	Mnemonic string                          `json:"mnemonic"`
	Networks map[chain.Network]*daemon.State `json:"networks"`
}

// Compare the same derived identity across networks, including profiles with no
// network database yet. Do not compare names, addresses, or imported profile IDs.
func backupIdentity(mnemonic string) (string, error) {
	keys, err := wallet.FromMnemonic(mnemonic)
	if err != nil {
		return "", errors.New("backup does not contain a valid wallet")
	}
	key, err := keys.Derive(2, "nostr-identity")
	if err != nil {
		return "", err
	}
	id := sha256.Sum256(key.PubKey().SerializeCompressed())
	return hex.EncodeToString(id[:]), nil
}

func validateBackupManifest(manifest *backupManifest) error {
	if manifest.FormatVersion != 1 || manifest.CreatedAt <= 0 || manifest.CreatedAt > time.Now().Add(24*time.Hour).Unix() || len(manifest.Wallets) == 0 || len(manifest.Wallets) > 20 {
		return errors.New("unsupported or invalid backup manifest")
	}
	identities, profiles := map[string]bool{}, map[string]bool{}
	for _, profile := range manifest.Wallets {
		if profile.ID == "" || len(profile.ID) > 128 || profiles[profile.ID] {
			return errors.New("invalid or repeated backup profile identifier")
		}
		profiles[profile.ID] = true
		if err := validateWalletName(profile.Name); err != nil {
			return fmt.Errorf("invalid backup wallet name: %w", err)
		}
		identity, err := backupIdentity(profile.Mnemonic)
		if err != nil || identity != profile.Identity || identities[identity] {
			return errors.New("invalid or duplicated wallet identity in backup")
		}
		identities[identity] = true
		if len(profile.Networks) == 0 || len(profile.Networks) > 3 {
			return errors.New("backup must describe each wallet's network state")
		}
		for network, state := range profile.Networks {
			if network == "" || !network.Valid() || state == nil || state.Version != 1 || state.Network.Normalized() != network || state.Mnemonic != profile.Mnemonic {
				return errors.New("backup network state does not match its wallet manifest")
			}
			if err := validateBackupState(state); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBackupState(state *daemon.State) error {
	for id, activity := range state.Activities {
		if activity.Version != 1 || activity.ID != id || activity.Network.Normalized() != state.Network.Normalized() || !activity.Chain.Valid() {
			return errors.New("invalid activity identity or network in backup")
		}
	}
	for id := range state.ActivityIndexes {
		if !id.Valid() {
			return errors.New("invalid activity history chain in backup")
		}
	}
	for id, swap := range state.Swaps {
		if swap == nil || swap.ID != id || (swap.Role != "maker" && swap.Role != "taker") {
			return errors.New("invalid swap in backup")
		}
	}
	for _, job := range state.TowerJobs {
		if job == nil {
			return errors.New("invalid watchtower job in backup")
		}
	}
	for _, delivery := range state.Outbox {
		if delivery == nil {
			return errors.New("invalid reliable message in backup")
		}
	}
	for id, send := range state.Sends {
		if send == nil || send.ID != id || !send.Chain.Valid() || send.Raw == "" {
			return errors.New("invalid pending payment in backup")
		}
	}
	for id := range state.ReceiveIndexes {
		if !id.Valid() {
			return errors.New("invalid receive chain in backup")
		}
	}
	normalizeState(state)
	return nil
}
