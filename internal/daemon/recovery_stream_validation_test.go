package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestStreamedRecoveryRequiresSourceForCompletedCheck(t *testing.T) {
	s := State{Version: StateVersion, Network: chain.Regtest}
	before := protocol.Digest(s)
	if err := PrepareStreamedRecovery(context.Background(), &s, storage.ArchiveStats{}, nil, time.Now().Unix(), false); err == nil {
		t.Fatal("an asserted empty archive replaced the authenticated source")
	}
	if protocol.Digest(s) != before {
		t.Fatal("missing-source refusal changed staged state")
	}
}

// The pinned source deliberately still contains the promoted cores. Only its
// identity indexes stay cold in the destination; traversal must not count both.
func TestStreamedRecoveryValidatesColdFundingCustody(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		t.Run(role, func(t *testing.T) {
			e, children, _ := ancestryGraph(t, role, chain.BTC)
			for _, child := range children {
				if err := e.stageArchive("swaps", child.ID); err != nil {
					t.Fatal(err)
				}
			}
			for id := range e.s.ParentOrders {
				if err := e.stageArchive("parent_orders", id); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			source, err := e.vault.Freeze()
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			var active State
			if _, _, err := source.LoadState(&active); err != nil {
				t.Fatal(err)
			}
			stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
			if err := source.VisitArchive(context.Background(), func(record storage.ArchiveRecord) error {
				promoted, err := PromoteRecoveryRecord(&active, record)
				if err != nil || promoted {
					return err
				}
				cold, _ := QuarantineArchiveRecord(record)
				raw, err := json.Marshal(cold)
				if err != nil {
					return err
				}
				stats.Count++
				stats.Bytes += uint64(len(raw) + 1)
				stats.Kinds[cold.Kind]++
				clear(raw)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			active.Capacity.Archived = stats
			if len(active.FillKeys) != 0 || stats.Kinds["fill_keys"] == 0 {
				t.Fatal("fixture did not preserve cold identity ownership")
			}
			encoded, err := json.Marshal(active)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(encoded)
			own, _, _ := localFunding(children[1])
			key := fundingIdentityKey(own.Chain, own.TxID)
			for _, mode := range []string{"valid", "missing-reader", "missing-key", "wrong-owner", "orphan-index", "lookup-error", "visitor-error", "cancelled", "late-cancel", "missing-promoted-core", "changed-promoted-core"} {
				t.Run(mode, func(t *testing.T) {
					var candidate State
					if err := json.Unmarshal(encoded, &candidate); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					reader := &streamedRecoveryFaultReader{source: source, mode: mode, key: key, other: children[0].ID, cancel: cancel}
					var input FillStateReader = reader
					switch mode {
					case "missing-reader":
						input = nil
					case "cancelled":
						cancel()
					case "missing-promoted-core":
						delete(candidate.Swaps, children[0].ID)
					case "changed-promoted-core":
						candidate.Swaps[children[0].ID].Stage = "different retained source display"
					}
					before, _ := json.Marshal(candidate)
					defer clear(before)
					err := PrepareStreamedRecovery(ctx, &candidate, stats, input, time.Now().Unix(), false)
					if mode != "valid" {
						if err == nil {
							t.Fatal("completed recovery accepted", mode)
						}
						after, _ := json.Marshal(candidate)
						defer clear(after)
						if string(before) != string(after) {
							t.Fatal("refused recovery changed the staged state")
						}
						if (mode == "cancelled" || mode == "late-cancel") && !errors.Is(err, context.Canceled) {
							t.Fatal("cancellation lost", err)
						}
						return
					}
					if err != nil {
						t.Fatal("valid promoted graph with cold indexes refused", err)
					}
					if len(candidate.FillKeys) != 0 || !reader.visited || reader.reads == 0 || candidate.Recovery == nil || candidate.Recovery.Status.State != "recovering" {
						t.Fatal("completed validation skipped cold custody or promoted index authority")
					}
					for _, child := range children {
						got := candidate.Swaps[child.ID]
						if got == nil || fundingCustodyHash(got) != fundingCustodyHash(child) || got.Secret != child.Secret {
							t.Fatal("recovery changed exact signed custody")
						}
						if len(got.FundingParents) > 0 && !got.FundingAncestryHeld {
							t.Fatal("cold index became current chain proof")
						}
						if role == "maker" {
							f := candidate.FillRecords[child.ID]
							p := candidate.ParentOrders[f.ParentID]
							if !f.ImportedUncertain || !p.RestoreHold || protocol.Digest(p.Fees) != protocol.Digest(active.ParentOrders[f.ParentID].Fees) || p.Quantities != active.ParentOrders[f.ParentID].Quantities {
								t.Fatal("recovery released quantity or monetary authorization")
							}
						}
					}
				})
			}
		})
	}
}

type streamedRecoveryFaultReader struct {
	source           FillStateReader
	mode, key, other string
	cancel           context.CancelFunc
	visited          bool
	reads            int
}

func (r *streamedRecoveryFaultReader) ReadArchive(kind, id string) (storage.ArchiveRecord, bool, error) {
	r.reads++
	if kind == "fill_keys" && id == r.key {
		if r.mode == "missing-key" {
			return storage.ArchiveRecord{}, false, nil
		}
		if r.mode == "lookup-error" {
			return storage.ArchiveRecord{}, false, errors.New("isolated cold lookup failure")
		}
	}
	row, ok, err := r.source.ReadArchive(kind, id)
	if ok && err == nil && row.Kind == "fill_keys" && row.ID == r.key && r.mode == "wrong-owner" {
		clear(row.Data)
		row.Data, err = json.Marshal(r.other)
	}
	return row, ok, err
}

func (r *streamedRecoveryFaultReader) VisitArchive(ctx context.Context, visit func(storage.ArchiveRecord) error) error {
	r.visited = true
	if r.mode == "visitor-error" {
		return errors.New("isolated archive traversal failure")
	}
	if err := r.source.VisitArchive(ctx, visit); err != nil {
		return err
	}
	if r.mode == "late-cancel" {
		r.cancel()
		return ctx.Err()
	}
	if r.mode == "orphan-index" {
		raw, _ := json.Marshal(r.other)
		defer clear(raw)
		return visit(storage.ArchiveRecord{Kind: "fill_keys", ID: fundingIdentityKey(chain.BTC, protocol.Digest("unreferenced forged funding")), Data: raw})
	}
	return nil
}
