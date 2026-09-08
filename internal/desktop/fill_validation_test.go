package desktop

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
	"github.com/blakeswap/blakeswap/internal/wallet"
	"github.com/btcsuite/btcd/btcec/v2"
)

func desktopConservationGraph(t *testing.T) daemon.State {
	t.Helper()
	seed, err := wallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	core := fixtureSwap(t, chain.Regtest, seed, "maker")
	keys := map[chain.ID]string{}
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		key, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[asset] = hex.EncodeToString(key.PubKey().SerializeCompressed())
	}
	terms, err := protocol.NewTerms(core.Request, keys, map[chain.ID]uint32{chain.BTC: 200, chain.Blake: 200})
	if err != nil {
		t.Fatal(err)
	}
	core.Terms = &terms
	core.Long, core.Short = terms.Long, terms.Short
	offer := terms.Offer()
	policy := daemon.FeeSelection{FundingFee: 2000}
	fees := map[chain.ID]int64{chain.BTC: 22000, chain.Blake: 2000}
	parent := &daemon.ParentOrder{Offer: offer, Economics: offer.EconomicsDigest(), FundingPolicy: policy, Quantities: daemon.QuantityLedger{Total: 1000000, Reserved: 1000000, Revision: 2}, Fees: map[chain.ID]daemon.FillBudget{}, Bounties: map[chain.ID]daemon.FillBudget{}}
	for _, asset := range []chain.ID{chain.BTC, chain.Blake} {
		parent.Fees[asset] = daemon.FillBudget{Limit: fees[asset], Reserved: fees[asset]}
		parent.Bounties[asset] = daemon.FillBudget{}
	}
	points := []daemon.CoinOutpoint{{TxID: transport.RandomID()}}
	child := &daemon.FillRecord{ID: core.ID, ParentID: offer.ID, ParentMaker: offer.Maker, ParentRevision: core.Request.Revision, RequestDigest: protocol.Digest(core.Request), Allocation: daemon.FillAllocation{Quantity: 1000000, Disposition: daemon.FillReserved}, BuyAmount: 2000000, FundingPolicy: policy, Fees: fees, Bounties: map[chain.ID]int64{chain.BTC: 0, chain.Blake: 0}, Inputs: points}
	state := daemon.State{Version: daemon.StateVersion, Network: chain.Regtest, Mnemonic: seed, ParentOrders: map[string]*daemon.ParentOrder{offer.ID: parent}, FillRecords: map[string]*daemon.FillRecord{core.ID: child}, Swaps: map[string]*daemon.Swap{core.ID: core}, FundingFees: map[string]daemon.FeeSelection{"swap/" + core.ID: policy}, CoinReservations: map[string]daemon.CoinReservation{"swap/" + core.ID: {Chain: chain.BTC, Inputs: points}}}
	fixtureIndexSwaps(t, &state)
	for _, key := range terms.MakerKeys {
		state.FillKeys["key/"+key] = core.ID
	}
	if err := daemon.ValidateCompleteFillState(&state); err != nil {
		t.Fatal("valid maker graph", err)
	}
	return state
}

func TestFillConservationStreamValidatesOnlyAfterCompleteArrival(t *testing.T) {
	for _, problem := range []string{"valid", "missing allocation", "counter mismatch"} {
		t.Run(problem, func(t *testing.T) {
			state := desktopConservationGraph(t)
			parent := state.ParentOrders[fixtureParentID]
			if problem == "counter mismatch" {
				b := parent.Fees[chain.BTC]
				b.Reserved--
				parent.Fees[chain.BTC] = b
			}
			var records []storage.ArchiveRecord
			add := func(kind, id string, value any) {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, storage.ArchiveRecord{Kind: kind, ID: id, Data: raw})
			}
			// Deliberately emit the core before its parent and child companions.
			add("swaps", fixtureChildID, state.Swaps[fixtureChildID])
			add("parent_orders", fixtureParentID, parent)
			add("funding_fees", "swap/"+fixtureChildID, state.FundingFees["swap/"+fixtureChildID])
			if problem != "missing allocation" {
				add("fill_records", fixtureChildID, state.FillRecords[fixtureChildID])
			}
			state.Swaps = nil
			state.ParentOrders = nil
			state.FillRecords = nil
			state.FundingFees = nil
			stats := storage.ArchiveStats{Kinds: map[string]uint64{}}
			for _, record := range records {
				raw, _ := json.Marshal(record)
				stats.Count++
				stats.Bytes += uint64(len(raw) + 1)
				stats.Kinds[record.Kind]++
			}
			state.Capacity = &daemon.CapacityRecord{Archived: stats}
			for _, record := range records {
				if err := validateStreamedRecord(state, record); err != nil {
					t.Fatal("single row required unavailable companions", err)
				}
			}
			staging, err := newPortableStaging(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer staging.close()
			delivered := 0
			source, err := staging.saveStream(context.Background(), state, stats, func(write func(storage.ArchiveRecord) error) error {
				for _, record := range records {
					if err := write(record); err != nil {
						return err
					}
					delivered++
				}
				return nil
			})
			if delivered != len(records) {
				t.Fatal("validation ran before completed arrival", delivered, err)
			}
			if problem != "valid" {
				if err == nil || source != nil {
					t.Fatal("invalid complete import obtained installable source")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored.db")
			if err := source.restore(context.Background(), target, []byte("separate target password"), 100, false); err != nil {
				t.Fatal(err)
			}
			v, err := storage.OpenReadOnly(target, []byte("separate target password"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var restored daemon.State
			if _, err := v.Load(&restored); err != nil {
				t.Fatal(err)
			}
			if !restored.ParentOrders[fixtureParentID].RestoreHold || !restored.FillRecords[fixtureChildID].ImportedUncertain || len(restored.Offers) != 0 {
				t.Fatal("validated import resumed publication or lost uncertainty")
			}
			if err := daemon.ValidateVaultProtocolState(v, &restored); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatal(err)
			}
		})
	}
}
