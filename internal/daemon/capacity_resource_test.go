package daemon

import (
	"math"
	"testing"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/storage"
)

func TestCapacitySeparatesLargeFinishedHistoryFromActiveAdmission(t *testing.T) {
	e, _ := receiveEngine(t)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.s.Capacity.Archived = storage.ArchiveStats{Count: 15000, Bytes: 512 << 20, Kinds: map[string]uint64{"sends": 1500, "tower_jobs": 1500, "seen": 12000}}
	// Maintained metadata is sufficient for health; this probe intentionally
	// supplies no cold bodies. Status must not scan or mutate history to compute it.
	e.capacityDiskAvailable = 10 << 30
	e.capacityDiskKnown = true
	h := e.capacityHealth()
	if !h.AdmissionAvailable || h.ActiveBytes != e.stateBytes || h.RetainedBytes != 512<<20 || h.StoredBytes != e.stateBytes+512<<20 || h.ReservedBytes != 3000*archivedEvidenceReserve {
		t.Fatal("finished history consumed active admission budget", h)
	}
	if !e.allowActivityGrowth(Activity{}, Activity{ID: "new-row"}) {
		t.Fatal("cold history permanently stopped new activity")
	}
	e.capacityDiskAvailable = h.ReservedBytes
	if e.capacityHealth().AdmissionAvailable {
		t.Fatal("continuation space ignored")
	}
	e.capacityDiskAvailable = 10 << 30
	e.stateBytes = WalletRecoveryBudget
	if e.capacityHealth().AdmissionAvailable {
		t.Fatal("oversized imported active state admitted unrelated work")
	}
}

func TestCapacityReservationsCoverReactivationAndCannotOverflow(t *testing.T) {
	e, _ := receiveEngine(t)
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.s.Capacity.Archived = storage.ArchiveStats{Count: math.MaxUint64, Bytes: math.MaxUint64, Kinds: map[string]uint64{"swaps": math.MaxUint64}}
	e.capacityDiskAvailable = math.MaxUint64
	e.capacityDiskKnown = true
	if h := e.capacityHealth(); h.ReservedBytes != math.MaxUint64 || h.StoredBytes != math.MaxUint64 || h.AdmissionAvailable {
		t.Fatal("archive resource arithmetic wrapped", h)
	}
	e.s.Capacity.Archived = storage.ArchiveStats{Kinds: map[string]uint64{}}
	e.s.Capacity.Reactivating = true
	if e.capacityHealth().AdmissionAvailable {
		t.Fatal("reactivating history admitted fresh work")
	}
	e.s.Capacity.Reactivating = false
	e.s.Capacity.Invalidated = map[string]bool{"send/old": true}
	e.s.Sends = map[string]*WalletSend{"old": {PublicSend: PublicSend{ID: "old", Chain: chain.BTC, Confirmations: 200}, Raw: "retained-signed"}}
	if h := e.capacityHealth(); h.ReservedBytes != activeRecoveryReserve || h.AdmissionAvailable {
		t.Fatal("terminal display erased reorg continuation reserve", h)
	}
}
