package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestFillDetailRetainsUnderlyingErrorAndAncestryMonitoring(t *testing.T) {
	for _, role := range []string{"maker", "taker"} {
		for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
			for _, held := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/held-%t", role, sell, held), func(t *testing.T) {
					e, children, _ := ancestryGraph(t, role, sell)
					s := children[1]
					if !held {
						if err := e.refreshFundingAncestry(context.Background(), s); err != nil {
							t.Fatal(err)
						}
					}
					// Include the provisional fee message literally inside a real stored
					// diagnostic; a string-removal fix would corrupt the underlying error.
					s.Error = "Exact child funding fee is unavailable. retained peer diagnostic"
					want := s.Error
					if held {
						want = errFundingAncestry.Error() + ". " + want
					}
					if got := e.publicSwap(s); got.Error != want || got.FundingFee != 2000 {
						t.Fatal("hot projection changed retained error", got.Error)
					}
					// Explicit storage placement tests read-only cold custody independently
					// of the canonical policy that normally qualifies deep compaction.
					if err := e.stageArchive("swaps", s.ID); err != nil {
						t.Fatal(err)
					}
					if err := e.save(); err != nil {
						t.Fatal(err)
					}
					before := protocol.Digest([]any{e.s, e.pendingArchive()})
					raw, _ := json.Marshal(map[string]string{"kind": "swap", "id": s.ID, "expected_wallet": e.Config.Name, "expected_network": "regtest"})
					detail, err := e.recordDetail(raw)
					if err != nil || detail.Swap == nil || !detail.Archived || detail.MonitoringRequired != held || detail.Swap.FundingFee != 2000 || detail.Swap.Error != want {
						t.Fatalf("cold detail altered error or held state: %+v %v", detail, err)
					}
					if before != protocol.Digest([]any{e.s, e.pendingArchive()}) {
						t.Fatal("detail lookup promoted or changed custody")
					}
					if e.s.Swaps[s.ID] != nil || e.s.FundingFees["swap/"+s.ID].FundingFee != 0 {
						t.Fatal("cold detail restored active core or fee")
					}
				})
			}
		}
	}
}

func TestFillDetailCannotHideMissingOrInvalidRetainedFee(t *testing.T) {
	for _, mode := range []string{"missing", "unreadable", "conflicting"} {
		t.Run(mode, func(t *testing.T) {
			e, children, _ := ancestryGraph(t, "maker", chain.BTC)
			s := children[1]
			s.Error = "retained peer diagnostic"
			if err := e.stageArchive("swaps", s.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
				row, found, err := e.vault.ReadArchive(kind, id)
				if kind == "funding_fees" && id == "swap/"+s.ID {
					switch mode {
					case "missing":
						return storage.ArchiveRecord{}, false, nil
					case "unreadable":
						return storage.ArchiveRecord{}, false, errors.New("injected unavailable fee record")
					default:
						var fee FeeSelection
						if err = json.Unmarshal(row.Data, &fee); err != nil {
							return row, false, err
						}
						fee.FundingFee++
						row.Data, err = json.Marshal(fee)
					}
				}
				return row, found, err
			}
			before := protocol.Digest([]any{e.s, e.pendingArchive()})
			raw, _ := json.Marshal(map[string]string{"kind": "swap", "id": s.ID, "expected_wallet": e.Config.Name, "expected_network": "regtest"})
			detail, err := e.recordDetail(raw)
			if err == nil || detail.Swap != nil {
				t.Fatal("invalid cold fee produced successful detail", detail)
			}
			if before != protocol.Digest([]any{e.s, e.pendingArchive()}) {
				t.Fatal("failed fee read changed custody")
			}
		})
	}
}
