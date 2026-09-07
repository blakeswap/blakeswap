package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestArchiveStreamingCopyRetainsCompleteCheckpointAndRejectsPartialPublication(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"), []byte("separate source vault password"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	records := make([]ArchiveRecord, 130)
	for i := range records {
		data, _ := json.Marshal(fmt.Sprintf("signed evidence %d", i))
		records[i] = ArchiveRecord{Kind: "signed", ID: fmt.Sprint(i), Data: data}
	}
	stats, err := source.CommitArchive(map[string]string{"active": "retained"}, ArchiveBatch{Put: records}, 0)
	if err != nil {
		t.Fatal(err)
	}
	view, err := source.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	var active map[string]string
	got, _, err := view.LoadState(&active)
	if err != nil || !archiveStatsEqual(stats, got) || active["active"] != "retained" {
		t.Fatal(got, active, err)
	}
	for _, mode := range []string{"complete", "interrupted", "duplicate", "wrong-total"} {
		t.Run(mode, func(t *testing.T) {
			target, err := Open(filepath.Join(t.TempDir(), "target.db"), []byte("separate staging vault password"))
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			expected := stats
			if mode == "wrong-total" {
				expected.Bytes++
			}
			count := 0
			err = target.ImportArchive(context.Background(), active, expected, func(write func(ArchiveRecord) error) error {
				return view.VisitArchive(context.Background(), func(record ArchiveRecord) error {
					count++
					if mode == "interrupted" && count == 80 {
						return errors.New("canceled after first private batch")
					}
					if err := write(record); err != nil {
						return err
					}
					if mode == "duplicate" && count == 70 {
						return write(record)
					}
					return nil
				})
			})
			var saved map[string]string
			if _, loadErr := target.Load(&saved); loadErr != nil {
				t.Fatal(loadErr)
			}
			if mode != "complete" {
				if err == nil || len(saved) != 0 {
					t.Fatal("partial staging published active checkpoint", mode, err, saved)
				}
				return
			}
			if err != nil || saved["active"] != "retained" {
				t.Fatal(err, saved)
			}
			archived, actual, err := target.LoadComplete(&saved, 0)
			if err != nil || len(archived) != len(records) || !archiveStatsEqual(stats, actual) {
				t.Fatal("stream copy lost records", actual, err)
			}
			if err := target.ImportArchive(context.Background(), active, stats, func(func(ArchiveRecord) error) error { return nil }); err == nil {
				t.Fatal("stream importer overwrote an existing state")
			}
		})
	}
}
