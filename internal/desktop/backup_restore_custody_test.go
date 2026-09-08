package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestStreamedRestoreKeepsSignedFundingOwnershipCold(t *testing.T) {
	manifest := portableManifest(t)
	state := manifest.Wallets[0].Networks[chain.Regtest]
	portableScaleCores(t, state, 1)
	portableScaleGrowFunding(t, state) // one synthetic, cryptographically signed transaction
	var child *daemon.Swap
	for _, swap := range state.Swaps {
		child = swap
	}
	key := "funding/" + string(child.Short.Chain) + "/" + child.Short.TxID
	state.FillKeys[key] = child.ID
	if err := daemon.ValidateCompleteFillState(state); err != nil {
		t.Fatal("valid signed source graph", err)
	}
	wantFunding, wantFees := child.ShortFunding, protocol.Digest(state.ParentOrders[child.Terms.Offer().ID].Fees)
	for id, value := range state.Swaps {
		appendPortableRecord(t, state, "swaps", id, value)
		delete(state.Swaps, id)
	}
	for id, value := range state.ParentOrders {
		appendPortableRecord(t, state, "parent_orders", id, value)
		delete(state.ParentOrders, id)
	}
	for id, value := range state.FillRecords {
		appendPortableRecord(t, state, "fill_records", id, value)
		delete(state.FillRecords, id)
	}
	for id, value := range state.FundingFees {
		appendPortableRecord(t, state, "funding_fees", id, value)
		delete(state.FundingFees, id)
	}
	for id, value := range state.FillKeys {
		appendPortableRecord(t, state, "fill_keys", id, value)
		delete(state.FillKeys, id)
	}
	staging, err := newPortableStaging(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer staging.close()
	source, err := staging.save(state)
	if err != nil {
		t.Fatal(err)
	}
	before, err := source.complete()
	if err != nil {
		t.Fatal(err)
	}
	beforeDigest := protocol.Digest(before)
	for _, cancelled := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		path := filepath.Join(t.TempDir(), "new-recovery.db")
		password := []byte("separate synthetic installation")
		err := source.restore(ctx, path, password, time.Now().Unix(), false)
		cancel()
		if cancelled {
			if !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled restore did not preserve cancellation", err)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatal("cancelled validation created an installable destination")
			}
			continue
		}
		if err != nil {
			t.Fatal("actual desktop streamed promotion refused cold funding index", err)
		}
		vault, err := storage.Open(path, password)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { vault.Close() })
		var got daemon.State
		if _, err := vault.Load(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.FillKeys) != 0 || got.Swaps[child.ID] == nil || got.Swaps[child.ID].ShortFunding != wantFunding || !got.FillRecords[child.ID].ImportedUncertain || !got.ParentOrders[child.Terms.Offer().ID].RestoreHold || got.Recovery == nil || got.Recovery.Status.State != "recovering" {
			t.Fatal("streamed restore changed custody or promoted authorization indexes")
		}
		if protocol.Digest(got.ParentOrders[child.Terms.Offer().ID].Fees) != wantFees {
			t.Fatal("streamed restore credited permanent fees")
		}
		row, found, err := vault.ReadArchive("fill_keys", key)
		if err != nil || !found {
			t.Fatal("cold funding identity missing", err)
		}
		var owner string
		err = json.Unmarshal(row.Data, &owner)
		clear(row.Data)
		if err != nil || owner != child.ID {
			t.Fatal("cold funding ownership changed", err)
		}
		if err := daemon.ValidateVaultProtocolState(vault, &got); err != nil {
			t.Fatal("installed complete graph invalid", err)
		}
	}
	after, err := source.complete()
	if err != nil || protocol.Digest(after) != beforeDigest {
		t.Fatal("restore changed its authenticated source", err)
	}
}
