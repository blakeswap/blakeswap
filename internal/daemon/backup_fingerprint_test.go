package daemon

import "testing"

func TestBackupFingerprintTracksRecoveryMaterialWithoutPollingNoise(t *testing.T) {
	state := State{Version: 1, Swaps: map[string]*Swap{"swap": {ID: "swap", Role: "maker", Secret: "secret", SelfRefunds: []string{"signed refund"}}}, TowerJobs: map[string]*TowerJob{"job": {LastAttempt: 1, Attempt: 2}}, Outbox: map[string]*Delivery{"message": {LastAttempt: 1}}}
	initial, err := BackupFingerprint(state)
	if err != nil {
		t.Fatal(err)
	}
	state.Swaps["swap"].Error = "temporary endpoint failure"
	state.Swaps["swap"].Stage = "waiting"
	state.Swaps["swap"].LongConfirmations = 200
	state.TowerJobs["job"].Confirmed = 100
	state.TowerJobs["job"].LastAttempt = 200
	state.TowerJobs["job"].Attempt = 3
	state.Outbox["message"].LastAttempt = 300
	state.EventTime = 1000
	same, err := BackupFingerprint(state)
	if err != nil || same != initial {
		t.Fatal("polling noise changes backup freshness", err)
	}
	state.Swaps["swap"].SecretExposed = true
	changed, err := BackupFingerprint(state)
	if err != nil || changed == initial {
		t.Fatal("secret exposure did not require a newer backup", err)
	}
	state.Swaps["swap"].SecretExposed = false
	state.Swaps["swap"].SelfRefunds = append(state.Swaps["swap"].SelfRefunds, "new signed variant")
	changed, err = BackupFingerprint(state)
	if err != nil || changed == initial {
		t.Fatal("new recovery variant did not change fingerprint", err)
	}
}

func TestBackupFreshnessRecordsExportedStateWithoutClaimingLaterChanges(t *testing.T) {
	state := State{Version: 1, Swaps: map[string]*Swap{"swap": {ID: "swap", Role: "maker"}}}
	status, err := StateBackupFreshness(state)
	if err != nil || !status.StateChanged || status.LastExportAt != 0 {
		t.Fatal("missing backup hidden", status, err)
	}
	fingerprint, err := BackupFingerprint(state)
	if err != nil {
		t.Fatal(err)
	}
	state.Backup = &BackupRecord{CreatedAt: 123, Fingerprint: fingerprint}
	status, err = StateBackupFreshness(state)
	if err != nil || status.StateChanged || status.LastExportAt != 123 {
		t.Fatal("successful backup remains dirty", status, err)
	}
	state.Swaps["swap"].SelfRefunds = []string{"transaction signed while export was encrypted"}
	status, err = StateBackupFreshness(state)
	if err != nil || !status.StateChanged || status.LastExportAt != 123 {
		t.Fatal("old snapshot marked newer state backed up", status, err)
	}
}
