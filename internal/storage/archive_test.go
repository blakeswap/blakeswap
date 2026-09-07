package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestArchiveAtomicMovementRestartAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	password := []byte("archive fixture password only")
	v, err := Open(path, password)
	if err != nil {
		t.Fatal(err)
	}
	secret := "retained preimage and signed refund evidence"
	active := map[string]string{"send": secret}
	if err = v.Save(active); err != nil {
		t.Fatal(err)
	}
	before := filepath.Join(t.TempDir(), "before.db")
	if err = v.Backup(before); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(secret)
	batch := ArchiveBatch{Put: []ArchiveRecord{{Kind: "send", ID: "private-send-id", Data: data}}}
	// Failure after the transaction has staged its archive changes must publish
	// neither half of the move, including its count/byte checkpoint.
	if _, err = v.CommitArchive(map[string]string{}, batch, 1); err == nil {
		t.Fatal("oversize transaction committed")
	}
	var current map[string]string
	if _, err = v.Load(&current); err != nil || current["send"] != secret {
		t.Fatal("failed compaction changed the active owner", current, err)
	}
	if _, found, err := v.ReadArchive("send", "private-send-id"); err != nil || found {
		t.Fatal("failed compaction published archive evidence", err)
	}
	stats, err := v.CommitArchive(map[string]string{}, batch, 1<<20)
	if err != nil || stats.Count != 1 || stats.Bytes <= uint64(len(data)) {
		t.Fatal(stats, err)
	}
	// Retrying a completed move is idempotent, including capacity accounting.
	again, err := v.CommitArchive(map[string]string{}, batch, 1<<20)
	if err != nil || again.Bytes != stats.Bytes || again.Count != 1 {
		t.Fatal("repeated transaction double-counted evidence", again, err)
	}
	after := filepath.Join(t.TempDir(), "after.db")
	if err = v.Backup(after); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("private-send-id")) {
		t.Fatal("archive leaked record identity or evidence", err)
	}
	for _, file := range []string{before, after, path} {
		v, err := Open(file, password)
		if err != nil {
			t.Fatal(err)
		}
		var restored map[string]string
		records, stats, err := v.LoadComplete(&restored, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if file == before {
			if restored["send"] != secret || len(records) != 0 || stats.Count != 0 {
				t.Fatal("pre-compaction backup lost active evidence")
			}
		} else {
			if len(restored) != 0 || len(records) != 1 || stats.Count != 1 || !bytes.Equal(records[0].Data, data) {
				t.Fatal("post-compaction backup lost archived evidence", restored, records)
			}
			if _, err = v.CommitArchive(active, ArchiveBatch{Delete: []ArchiveKey{{"send", "private-send-id"}}}, 1<<20); err != nil {
				t.Fatal(err)
			}
			if _, found, err := v.ReadArchive("send", "private-send-id"); err != nil || found {
				t.Fatal("reactivation retained two owners", err)
			}
		}
		if err = v.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiveBindingCollisionAndEncodedBudget(t *testing.T) {
	v, err := Open(filepath.Join(t.TempDir(), "state.db"), []byte("archive binding fixture password"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	// Quotes, HTML and newlines expand under JSON encoding. Count the portable
	// envelope, not the source payload length, before allowing a write.
	data, _ := json.Marshal("\"<>&\n")
	a := ArchiveRecord{Kind: "receipt", ID: "a", Data: data}
	b := ArchiveRecord{Kind: "receipt", ID: "b", Data: data}
	encoded, _ := json.Marshal(a)
	if _, err = v.CommitArchive(struct{}{}, ArchiveBatch{Put: []ArchiveRecord{a}}, uint64(len(encoded)+2)); err == nil {
		t.Fatal("portable record separator was not budgeted")
	}
	if _, err = v.CommitArchive(struct{}{}, ArchiveBatch{Put: []ArchiveRecord{a, b}}, 1<<20); err != nil {
		t.Fatal(err)
	}
	a.Data = json.RawMessage(`"different authorization"`)
	if _, err = v.CommitArchive(struct{}{}, ArchiveBatch{Put: []ArchiveRecord{a}}, 1<<20); err == nil {
		t.Fatal("collision overwrote retained authorization")
	}
	if err = v.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(archiveBucket)
		return bucket.Put(v.archiveIndex("receipt", "a"), bytes.Clone(bucket.Get(v.archiveIndex("receipt", "b"))))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = v.ReadArchive("receipt", "a"); err == nil {
		t.Fatal("ciphertext accepted under another record identity")
	}
	if _, _, err = v.LoadComplete(&struct{}{}, 1<<20); err == nil {
		t.Fatal("corrupt archive exported as a complete backup")
	}
}

func TestArchiveBoundedPagesBeyondLifetimeCaps(t *testing.T) {
	v, err := Open(filepath.Join(t.TempDir(), "state.db"), []byte("large archive fixture password"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	batch := ArchiveBatch{}
	for _, kind := range []string{"send", "offer", "tower", "seen"} {
		count := 1100
		if kind == "seen" {
			count = 10010
		}
		for i := range count {
			batch.Put = append(batch.Put, ArchiveRecord{Kind: kind, ID: fmt.Sprintf("%s-%d", kind, i), Data: json.RawMessage(`{"retained":"signed evidence"}`)})
		}
	}
	stats, err := v.CommitArchive(struct{}{}, batch, 64<<20)
	if err != nil || stats.Count != 13310 {
		t.Fatal(stats, err)
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, next, err := v.ArchivePage("seen", cursor, 137)
		if err != nil || len(page) > 137 {
			t.Fatal(err)
		}
		for _, record := range page {
			if seen[record.ID] || record.Kind != "seen" {
				t.Fatal("page duplicated or crossed a record category")
			}
			seen[record.ID] = true
		}
		cursor = next
		if cursor == "" {
			break
		}
	}
	if len(seen) != 10010 {
		t.Fatal("pagination silently skipped archived identity", len(seen))
	}
}
