package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestParentFillArchiveInitialActivityVariantsStable(t *testing.T) {
	e, _, _ := fillAdmissionEngine(t, chain.BTC)
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	row := func() Activity {
		return Activity{ID: "index-activity", Kind: "swap_refund", Chain: chain.BTC, Direction: "incoming", TxID: a, Status: "prepared", LocalStatus: "prepared", Amount: 12000, Principal: 15000, Fee: 3000, FeeKnown: true,
			Variants: []string{b, a}, VariantAmounts: []ActivityVariant{{TxID: b, Amount: 11000, Fee: 4000}, {TxID: a, Amount: 12000, Fee: 3000}}}
	}
	e.putActivity(row(), false)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	before, err := BackupFingerprint(e.s)
	if err != nil {
		t.Fatal(err)
	}
	token, revision := BackupSemanticToken(e.s), e.s.ActivityRevision
	e.putActivity(row(), false)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	after, err := BackupFingerprint(e.s)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || BackupSemanticToken(e.s) != token || e.s.ActivityRevision != revision || len(e.s.Activities["index-activity"].History) != 0 {
		t.Fatal("replaying unchanged first-insertion variants changed backup semantics or added an outcome")
	}
	stored := e.s.Activities["index-activity"]
	if len(stored.Variants) != 2 || stored.Variants[0] != a || stored.Variants[1] != b || len(stored.VariantAmounts) != 2 || stored.VariantAmounts[0].TxID != a || stored.VariantAmounts[0].Fee != 3000 || stored.VariantAmounts[1].TxID != b || stored.VariantAmounts[1].Fee != 4000 {
		t.Fatal("canonical variants lost their exact aligned amounts")
	}
	changed := row()
	changed.Variants = append(changed.Variants, c)
	changed.VariantAmounts = append(changed.VariantAmounts, ActivityVariant{TxID: c, Amount: 10000, Fee: 5000})
	e.putActivity(changed, false)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	after, err = BackupFingerprint(e.s)
	if err != nil {
		t.Fatal(err)
	}
	if after == before || BackupSemanticToken(e.s) == token || e.s.ActivityRevision != revision+1 || len(e.s.Activities["index-activity"].History) != 1 {
		t.Fatal("a distinct signed variant did not retain its outcome and change backup coverage")
	}
}

func TestParentFillArchiveMovesIdentityIndexesWithExactChild(t *testing.T) {
	for _, sell := range []chain.ID{chain.BTC, chain.Blake} {
		t.Run(string(sell), func(t *testing.T) {
			e, children, secrets := fundedFillPair(t, sell)
			all := fillPairOutcomes(t, e, children, secrets, false)
			for _, s := range children {
				if err := e.advanceSwap(context.Background(), s, all); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			s := children[0]
			keys, err := swapIdentityKeys(s)
			if err != nil {
				t.Fatal(err)
			}
			other, err := swapIdentityKeys(children[1])
			if err != nil {
				t.Fatal(err)
			}
			fingerprint, err := BackupFingerprint(e.s)
			if err != nil {
				t.Fatal(err)
			}
			token := BackupSemanticToken(e.s)
			if err := e.stageArchive("swaps", s.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				if e.s.FillKeys[key] != "" {
					t.Fatal("cold child retained lifetime hot identity", key)
				}
				record, found, err := e.vault.ReadArchive("fill_keys", key)
				var owner string
				if err != nil || !found || json.Unmarshal(record.Data, &owner) != nil || owner != s.ID {
					t.Fatal("archive lost exact child identity", key, err)
				}
			}
			for _, key := range other {
				if e.s.FillKeys[key] != children[1].ID {
					t.Fatal("another live child lost index custody")
				}
			}
			complete, err := LoadCompleteState(e.vault)
			if err != nil {
				t.Fatal(err)
			}
			after, err := BackupFingerprint(complete)
			if err != nil {
				t.Fatal(err)
			}
			if after != fingerprint || BackupSemanticToken(e.s) != token {
				t.Fatal("archive placement changed backup semantics")
			}
			// A complete serialized checkpoint installs every cold index and its
			// active checkpoint together in a separate encrypted vault.
			encoded, err := json.Marshal(complete)
			if err != nil {
				t.Fatal(err)
			}
			var portable State
			if err := json.Unmarshal(encoded, &portable); err != nil {
				t.Fatal(err)
			}
			clear(encoded)
			path := filepath.Join(t.TempDir(), "roundtrip.db")
			password := []byte("disposable index roundtrip")
			if err := storage.Initialize(path, password, portable); err != nil {
				t.Fatal(err)
			}
			if err := PreflightStateVersion(path, password); err != nil {
				t.Fatal("complete cold index preflight", err)
			}
			v, restored, err := openCurrentStateVault(path, password)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			roundtrip, err := LoadCompleteState(v)
			if err != nil {
				t.Fatal(err)
			}
			after, err = BackupFingerprint(roundtrip)
			if err != nil || after != fingerprint || BackupSemanticToken(restored) != token || restored.Swaps[s.ID] != nil {
				t.Fatal("complete roundtrip changed backup semantics or promoted cold core", err)
			}
			for _, key := range keys {
				if restored.FillKeys[key] != "" {
					t.Fatal("complete roundtrip made the immutable index hot")
				}
			}
			before := protocol.Digest(e.s.ParentOrders)
			if err := e.retainSwapIdentity(s); err != nil {
				t.Fatal(err)
			}
			if e.s.Swaps[s.ID] != nil || protocol.Digest(e.s.ParentOrders) != before {
				t.Fatal("cold identity check reactivated authority")
			}
			for _, key := range keys {
				if e.s.FillKeys[key] != "" {
					t.Fatal("cold equality copied identity hot")
				}
			}
			if _, err := e.activateArchived("swaps", s.ID); err != nil {
				t.Fatal(err)
			}
			if e.s.FillRecords[s.ID] == nil {
				t.Fatal("reactivation lost allocation")
			}
			for _, key := range keys {
				if e.s.FillKeys[key] != "" {
					t.Fatal("reactivation unnecessarily promoted immutable index")
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				if err := e.stageArchive("swaps", child.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			complete, err = LoadCompleteState(e.vault)
			if err != nil {
				t.Fatal(err)
			}
			after, err = BackupFingerprint(complete)
			if err != nil || after != fingerprint || BackupSemanticToken(e.s) != token || len(e.s.FillKeys) != 0 || e.s.Capacity.Archived.Kinds["fill_keys"] != uint64(len(keys)+len(other)) {
				t.Fatal("repeated core placement duplicated indexes or changed complete backup semantics", err)
			}
		})
	}
}

func TestParentFillArchiveFailedSaveKeepsAllHotOwnersUntilRetry(t *testing.T) {
	e, children, _ := fundedFillPair(t, chain.BTC)
	s := children[0]
	keys, err := swapIdentityKeys(s)
	if err != nil {
		t.Fatal(err)
	}
	before, err := BackupFingerprint(e.s)
	if err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(e.s)
	if err := e.stageArchive("swaps", s.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.vault.PrivateDirectory(), "state.db")
	if err := e.vault.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err == nil || e.fatal == nil {
		t.Fatal("failed archive commit did not stop execution")
	}
	v, err := storage.OpenExisting(path, []byte("receive-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var saved State
	if _, err := v.Load(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Swaps[s.ID] == nil || saved.FillRecords[s.ID] == nil || saved.FundingFees["swap/"+s.ID].FundingFee != 6500 || BackupSemanticToken(saved) != token {
		t.Fatal("failed archive commit split prior active companions")
	}
	for _, key := range keys {
		if saved.FillKeys[key] != s.ID {
			t.Fatal("failed commit lost an original hot index")
		}
		if _, found, err := v.ReadArchive("fill_keys", key); err != nil || found {
			t.Fatal("failed commit published a cold index", err)
		}
	}
	if err := ValidateVaultProtocolState(v, &saved); err != nil {
		t.Fatal("failed commit left an invalid ownership checkpoint", err)
	}
	e = reopenedFixtureEngine(t, e, v, saved)
	if err := e.stageArchive("swaps", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	complete, err := LoadCompleteState(v)
	if err != nil {
		t.Fatal(err)
	}
	after, err := BackupFingerprint(complete)
	if err != nil || after != before || BackupSemanticToken(e.s) != token {
		t.Fatal("successful placement retry changed logical backup material", err)
	}
	for _, key := range keys {
		if e.s.FillKeys[key] != "" {
			t.Fatal("successful retry retained a hot index")
		}
		if _, found, err := v.ReadArchive("fill_keys", key); err != nil || !found {
			t.Fatal("successful retry lost a cold index", err)
		}
	}
}

func TestParentFillArchiveTakerKeepsSecretIdentityAndExactReceipt(t *testing.T) {
	e, p := tradeFixture(t, "taker")
	quote := requestQuote(t, e, p)
	request := confirmation(quote)
	accepted := confirmQuote(t, e, request)
	s := e.s.Swaps[accepted.ID]
	if s == nil || s.Secret == "" || s.Terms != nil {
		t.Fatal("fixture did not retain its locally generated pre-acceptance secret")
	}
	keys, err := swapIdentityKeys(s)
	if err != nil || len(keys) != 3 {
		t.Fatal("wrong pre-acceptance identity set", err)
	}
	before, err := BackupFingerprint(e.s)
	if err != nil {
		t.Fatal(err)
	}
	token := BackupSemanticToken(e.s)
	// Exercise the ownership primitive; production eligibility remains the
	// separate lifecycle/compaction decision, never an absence inference here.
	if err := e.stageArchive("swaps", s.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	state := protocol.Digest(e.s)
	if again := confirmQuote(t, e, request); again != accepted || protocol.Digest(e.s) != state || e.s.Swaps[s.ID] != nil {
		t.Fatal("exact receipt retry changed cold child authority")
	}
	other := *s
	other.ID = protocol.Digest("another retained child")
	other.Request.ID = other.ID
	if err := e.retainSwapIdentity(&other); err == nil || protocol.Digest(e.s) != state {
		t.Fatal("cold taker secret identity became available to another child")
	}
	for _, key := range keys {
		if e.s.FillKeys[key] != "" {
			t.Fatal("taker retained lifetime hot identity")
		}
		var owner string
		if found, err := e.archivedValue("fill_keys", key, &owner); err != nil || !found || owner != s.ID {
			t.Fatal("taker lost exact cold identity", err)
		}
	}
	complete, err := LoadCompleteState(e.vault)
	if err != nil {
		t.Fatal(err)
	}
	after, err := BackupFingerprint(complete)
	if err != nil || after != before || BackupSemanticToken(e.s) != token {
		t.Fatal("taker archival changed backup coverage", err)
	}
}

func TestParentFillArchivePlansEveryIdentityBeforeChangingOwnership(t *testing.T) {
	e, children, _ := fundedFillPair(t, chain.BTC)
	s := children[0]
	keys, err := swapIdentityKeys(s)
	if err != nil {
		t.Fatal(err)
	}
	last := keys[len(keys)-1]
	for _, problem := range []string{"read failure", "duplicate cold owner"} {
		before := protocol.Digest(e.s)
		e.archiveRead = func(kind, id string) (storage.ArchiveRecord, bool, error) {
			if kind == "fill_keys" && id == last {
				if problem == "read failure" {
					return storage.ArchiveRecord{}, false, errors.New("injected final index read failure")
				}
				data, _ := json.Marshal(children[1].ID)
				return storage.ArchiveRecord{Kind: kind, ID: id, Data: data}, true, nil
			}
			return e.vault.ReadArchive(kind, id)
		}
		if err := e.stageArchive("swaps", s.ID); err == nil {
			t.Fatal("archive ignored incomplete identity custody", problem)
		}
		if protocol.Digest(e.s) != before || len(e.archivePuts) != 0 || len(e.archiveDeletes) != 0 {
			t.Fatal("failed final identity plan moved earlier core/companions", problem)
		}
		e.archiveRead = nil
	}
}
