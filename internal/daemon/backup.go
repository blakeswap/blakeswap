package daemon

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/blakeswap/blakeswap/internal/storage"
)

// FreezeBackup pins a committed view without copying lifetime archive bodies.
// The caller must release it; Close joins this reader before closing the vault.
func (e *Engine) FreezeBackup() (*storage.ReadSnapshot, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.save(); err != nil {
		return nil, nil, err
	}
	view, err := e.vault.Freeze()
	if err != nil {
		return nil, nil, err
	}
	e.activityReaders.Add(1)
	var once sync.Once
	return view, func() { once.Do(func() { _ = view.Close(); e.activityReaders.Done() }) }, nil
}

// BackupSnapshot owns a deep copy of the complete durable state. Desktop stops
// its workers and joins advisory reads first to capture all selected wallets at
// one lifecycle boundary, then takes this lock to exclude direct commands.
func (e *Engine) BackupSnapshot() (State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	raw, err := json.Marshal(e.s)
	if err != nil {
		return State{}, err
	}
	defer clear(raw)
	var snapshot State
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		return State{}, err
	}
	if e.vault != nil {
		if len(e.archivePuts) != 0 || len(e.archiveDeletes) != 0 {
			return State{}, errors.New("wallet archive checkpoint is not yet durable")
		}
		stored, err := LoadCompleteState(e.vault)
		if err != nil {
			return State{}, err
		}
		snapshot.Archive = stored.Archive
	}
	return snapshot, nil
}

// BackupRecord describes a successfully published portable archive. Its digest
// identifies the exported recovery material, even if the live wallet changes
// while encryption/filesystem IO completes outside the lifecycle lock.
type BackupRecord struct {
	SemanticToken string `json:"semantic_token,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	Fingerprint   string `json:"fingerprint"`
}

type BackupFreshness struct {
	LastExportAt int64  `json:"last_export_at"`
	StateChanged bool   `json:"state_changed"`
	Reminder     string `json:"reminder"`
}

func (e *Engine) RecordBackup(fingerprint string, createdAt int64) error {
	return e.RecordBackupSnapshot(fingerprint, "", createdAt)
}
func (e *Engine) RecordBackupSnapshot(fingerprint, semantic string, createdAt int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(fingerprint) != 64 || (semantic != "" && len(semantic) != 64) || createdAt <= 0 {
		return errors.New("invalid backup record")
	}
	if semantic == "" && fingerprint == e.backupFingerprint {
		semantic = BackupSemanticToken(e.s)
	}
	e.s.Backup = &BackupRecord{CreatedAt: createdAt, Fingerprint: fingerprint, SemanticToken: semantic}
	return e.save()
}

func StateBackupFreshness(state State) (BackupFreshness, error) {
	out := BackupFreshness{StateChanged: true, Reminder: "Create a portable state backup; a recovery phrase cannot recover random swap preimages or signed rescue transactions."}
	if state.Backup == nil {
		return out, nil
	}
	if state.Backup.SemanticToken != "" && BackupSemanticToken(state) != "" {
		return backupFreshness(state, ""), nil
	}
	current, err := BackupFingerprint(state)
	if err != nil {
		return out, err
	}
	return backupFreshness(state, current), nil
}

func backupFreshness(state State, current string) BackupFreshness {
	out := BackupFreshness{StateChanged: true, Reminder: "Create a portable state backup; a recovery phrase cannot recover random swap preimages or signed rescue transactions."}
	if state.Backup == nil {
		return out
	}
	out.LastExportAt = state.Backup.CreatedAt
	if state.Backup.SemanticToken != "" && BackupSemanticToken(state) != "" {
		out.StateChanged = state.Backup.SemanticToken != BackupSemanticToken(state)
	} else {
		out.StateChanged = current == "" || current != state.Backup.Fingerprint
	}
	out.Reminder = ""
	if out.StateChanged {
		out.Reminder = "Recovery state changed since the last portable export. Create a new state backup."
	}
	return out
}
