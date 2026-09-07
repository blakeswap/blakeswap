package storage

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type crashCheckpoint struct {
	Phase string `json:"phase"`
	stop  bool
}

func (c crashCheckpoint) ValidateArchiveCheckpoint(ArchiveStats) error {
	if c.stop {
		os.Exit(23)
	}
	return nil
}
func TestArchiveProcessKillCheckpointChild(t *testing.T) {
	path := os.Getenv("BLAKESWAP_ARCHIVE_CRASH_PATH")
	if path == "" {
		t.Skip("child process only")
	}
	vault, err := Open(path, []byte("isolated process kill fixture password"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal("retained signed refund and secret knowledge")
	_, err = vault.CommitArchive(crashCheckpoint{Phase: "archive-owned", stop: os.Getenv("BLAKESWAP_ARCHIVE_CRASH_POINT") == "before"}, ArchiveBatch{Put: []ArchiveRecord{{Kind: "signed", ID: "retained-obligation", Data: raw}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(24)
}
func TestArchiveProcessKillPreservesExactlyOneCompleteOwner(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []string{"before", "after"} {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			password := []byte("isolated process kill fixture password")
			vault, err := Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			if err = vault.Save(crashCheckpoint{Phase: "active-owned"}); err != nil {
				t.Fatal(err)
			}
			if err = vault.Close(); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(executable, "-test.run=^TestArchiveProcessKillCheckpointChild$")
			command.Env = append(os.Environ(), "BLAKESWAP_ARCHIVE_CRASH_PATH="+path, "BLAKESWAP_ARCHIVE_CRASH_POINT="+point)
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			expected := 23
			if point == "after" {
				expected = 24
			}
			if !ok || exit.ExitCode() != expected {
				t.Fatalf("child did not exit at %s boundary: %v %s", point, err, output)
			}
			vault, err = Open(path, password)
			if err != nil {
				t.Fatal(err)
			}
			defer vault.Close()
			var state crashCheckpoint
			if _, err = vault.Load(&state); err != nil {
				t.Fatal(err)
			}
			record, found, err := vault.ReadArchive("signed", "retained-obligation")
			if err != nil {
				t.Fatal(err)
			}
			if point == "before" {
				if state.Phase != "active-owned" || found {
					t.Fatal("uncommitted move survived process death", state, found)
				}
			} else {
				if state.Phase != "archive-owned" || !found || string(record.Data) != `"retained signed refund and secret knowledge"` {
					t.Fatal("committed move lost recovery evidence", state, found)
				}
			}
		})
	}
}
