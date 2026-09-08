package storage

import (
	"context"
	"encoding/json"
	"errors"
	bolt "go.etcd.io/bbolt"
	"testing"
)

func TestArchiveKindSnapshotSkipsUnrelatedBodiesAndFencesCompletion(t *testing.T) {
	v, records := captureFixture(t, 130)
	extra := ArchiveRecord{Kind: "unrelated", ID: "bad-cipher", Data: json.RawMessage(`"private"`)}
	if _, err := v.CommitArchive(map[string]string{"checkpoint": "original"}, ArchiveBatch{Put: []ArchiveRecord{extra}}, 0); err != nil {
		t.Fatal(err)
	}
	// A selected-category query must not read/decrypt unrelated raw evidence.
	if err := v.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(archiveBucket).Put(v.archiveIndex(extra.Kind, extra.ID), []byte("malformed-unrelated-cipher"))
	}); err != nil {
		t.Fatal(err)
	}
	s := fallbackCapture(t, v)
	count := 0
	if err := s.VisitArchiveKind(context.Background(), "receipt", func(record ArchiveRecord) error { count++; return nil }); err != nil || count != 130 {
		t.Fatal(count, err)
	}
	if _, found, err := s.ReadArchive("receipt", records[0].ID); err != nil || !found {
		t.Fatal(found, err)
	}
	if err := s.VisitArchive(context.Background(), func(ArchiveRecord) error { return nil }); err == nil {
		t.Fatal("complete scan accepted unrelated corruption")
	}
	// Delete/reinsert the same selected identity after the final callback. Its
	// count and bytes return to the original values; generation must still fail.
	if err := s.VisitArchiveKind(context.Background(), "receipt", func(record ArchiveRecord) error {
		count--
		if count != 0 {
			return nil
		}
		if _, err := v.CommitArchive(map[string]string{}, ArchiveBatch{Delete: []ArchiveKey{{Kind: record.Kind, ID: record.ID}}}, 0); err != nil {
			return err
		}
		_, err := v.CommitArchive(map[string]string{}, ArchiveBatch{Put: []ArchiveRecord{record}}, 0)
		return err
	}); !errors.Is(err, ErrArchiveChanged) {
		t.Fatal("final callback ABA accepted", err)
	}
	if _, _, err := s.ReadArchive("receipt", records[0].ID); !errors.Is(err, ErrArchiveChanged) {
		t.Fatal("exact read lost generation fence", err)
	}
}
