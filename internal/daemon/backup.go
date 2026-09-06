package daemon

import (
	"encoding/json"
	"errors"
)

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
	err = json.Unmarshal(raw, &snapshot)
	return snapshot, err
}

// BackupRecord describes a successfully published portable archive. Its digest
// identifies the exported recovery material, even if the live wallet changes
// while encryption/filesystem IO completes outside the lifecycle lock.
type BackupRecord struct {
	CreatedAt   int64  `json:"created_at"`
	Fingerprint string `json:"fingerprint"`
}

type BackupFreshness struct {
	LastExportAt int64  `json:"last_export_at"`
	StateChanged bool   `json:"state_changed"`
	Reminder     string `json:"reminder"`
}

func (e *Engine) RecordBackup(fingerprint string, createdAt int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(fingerprint) != 64 || createdAt <= 0 {
		return errors.New("invalid backup record")
	}
	e.s.Backup = &BackupRecord{CreatedAt: createdAt, Fingerprint: fingerprint}
	return e.save()
}

func StateBackupFreshness(state State) (BackupFreshness, error) {
	out := BackupFreshness{StateChanged: true, Reminder: "Create a portable state backup; a recovery phrase cannot recover random swap preimages or signed rescue transactions."}
	if state.Backup == nil {
		return out, nil
	}
	current, err := BackupFingerprint(state)
	if err != nil {
		return out, err
	}
	out.LastExportAt = state.Backup.CreatedAt
	out.StateChanged = current != state.Backup.Fingerprint
	out.Reminder = ""
	if out.StateChanged {
		out.Reminder = "Recovery state changed since the last portable export. Create a new state backup."
	}
	return out, nil
}
