package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestArchiveSemanticFreshnessSurvivesMovesChangesAndReopen(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Swaps = map[string]*Swap{"settled": {ID: "settled", Role: "maker", Stage: "completed", SecretObserved: true, SelfClaims: []string{"retained signed transaction"}}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := BackupFingerprint(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(snapshot)
	if err = e.RecordBackupSnapshot(fingerprint, token, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if e.Status().Backup.StateChanged {
		t.Fatal("frozen snapshot was not recorded")
	}
	if err = e.stageArchive("swaps", "settled"); err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if e.s.Version != 2 || BackupSemanticToken(e.s) != token || e.Status().Backup.StateChanged {
		t.Fatal("a storage-only move changed semantic freshness")
	}
	archived, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	after, err := BackupFingerprint(archived)
	if err != nil || after != fingerprint || len(archived.Archive) != 1 {
		t.Fatal("portable canonical fingerprint changed after compaction", after, fingerprint, err)
	}
	if _, err := BackupFingerprint(e.s); err == nil {
		t.Fatal("active-only state claimed a complete fingerprint")
	}
	if _, err = e.activateArchived("swaps", "settled"); err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) != token {
		t.Fatal("reactivation changed semantic freshness")
	}
	e.s.Swaps["settled"].SelfClaims = append(e.s.Swaps["settled"].SelfClaims, "new signed variant")
	if err = e.stageArchive("swaps", "settled"); err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) == token || !e.Status().Backup.StateChanged {
		t.Fatal("meaningful change immediately archived was missed")
	}
	current := BackupSemanticToken(e.s)
	// A slow export of the old frozen snapshot must not mark the newer signed
	// variant as backed up when its completion arrives later.
	if err = e.RecordBackupSnapshot(fingerprint, token, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if !e.Status().Backup.StateChanged || BackupSemanticToken(e.s) != current {
		t.Fatal("old export covered a newer semantic change")
	}
	var saved State
	if _, err = e.vault.Load(&saved); err != nil {
		t.Fatal(err)
	}
	e.s, e.semanticParts = saved, nil
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) != current {
		t.Fatal("reopen changed the committed semantic token")
	}
}

func TestArchiveImportRejectsOverlapAndRetainsCompleteRecovery(t *testing.T) {
	e, _ := receiveEngine(t)
	e.s.Seen = map[string]string{"authenticated-sender:message-id": "content-digest"}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.stageArchive("seen", "authenticated-sender:message-id"); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.BackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateArchiveState(snapshot); err != nil {
		t.Fatal(err)
	}
	v, err := storage.Open(filepath.Join(t.TempDir(), "state.db"), []byte("portable archive import fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err = v.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCompleteState(v)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := CompleteState(loaded)
	if err != nil || complete.Seen["authenticated-sender:message-id"] != "content-digest" {
		t.Fatal("import lost authenticated replay identity", err)
	}
	loaded.Seen["authenticated-sender:message-id"] = "content-digest"
	if err := ValidateArchiveState(loaded); err == nil {
		t.Fatal("duplicate active/archive ownership accepted")
	}
	loaded.Seen = nil
	loaded.Archive[0].Data = json.RawMessage(`null`)
	if err := ValidateArchiveState(loaded); err == nil {
		t.Fatal("malformed archived replay record accepted")
	}
}

func TestArchiveFailedSaveCannotPublishANewerFreshnessToken(t *testing.T) {
	e, _ := receiveEngine(t)
	_ = e.vault.Close()
	path := filepath.Join(t.TempDir(), "state.db")
	password := []byte("isolated failure/reopen password")
	vault, err := storage.Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	e.vault = vault
	e.s.Seen = map[string]string{"sender:original": "original digest"}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(e.s)
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	e.s.Seen["sender:original"] = "changed meaningful evidence"
	if err = e.stageArchive("seen", "sender:original"); err == nil {
		t.Fatal("closed vault unexpectedly allowed archive read")
	}
	if err = e.save(); err == nil || e.fatal == nil {
		t.Fatal("failed save did not stop execution")
	}
	reopened, err := storage.Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var saved State
	if _, err = reopened.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Seen["sender:original"] != "original digest" || BackupSemanticToken(saved) != token {
		t.Fatal("failed save changed durable evidence or freshness")
	}
	e.vault, e.s, e.fatal, e.semanticParts = reopened, saved, nil, nil
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if BackupSemanticToken(e.s) != token {
		t.Fatal("reopen lost the successful semantic checkpoint")
	}
}
