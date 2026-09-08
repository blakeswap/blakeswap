package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func (e *Engine) nextHistoryCoverage(ctx context.Context, id chain.ID) (HistoryCoverage, error) {
	point := e.archiveCurrent[id]
	source, ok := e.nodes[id].(recoveryBlockHasher)
	if !ok || point.Hash == "" || !e.activitySourceCurrent(id, point.Generation) {
		return HistoryCoverage{}, errors.New("history coverage source unavailable")
	}
	prior := HistoryCoverage{}
	if e.s.Capacity != nil {
		prior = e.s.Capacity.HistoryCoverage[id]
	}
	if err := ctx.Err(); err != nil {
		return HistoryCoverage{}, err
	}
	// The caller just verified this selected row and the exact current tip. A
	// matching prefix (or the first one) needs no redundant per-row RPC.
	if prior.ID == "" || prior.Hash == "" {
		return HistoryCoverage{ID: transport.RandomID(), Height: point.Height, Hash: point.Hash}, nil
	}
	if prior.Height == point.Height && prior.Hash == point.Hash {
		return prior, nil
	}
	same := false
	if prior.ID != "" && prior.Hash != "" && prior.Height <= point.Height {
		hash, err := source.BlockHash(ctx, prior.Height)
		if err != nil || hash == "" {
			return HistoryCoverage{}, errors.Join(err, errors.New("history prefix verification unavailable"))
		}
		same = hash == prior.Hash
	}
	tip, err := source.BlockHash(ctx, point.Height)
	if err != nil || tip != point.Hash || !e.activitySourceCurrent(id, point.Generation) {
		return HistoryCoverage{}, errors.Join(err, errors.New("history prefix source changed"))
	}
	if !same {
		err := e.invalidateArchive("A canonical block contradicted retained historical inclusion.")
		return HistoryCoverage{}, errors.Join(err, errors.New("historical inclusion is being reactivated before a new prefix can be established"))
	}
	return HistoryCoverage{ID: prior.ID, Height: point.Height, Hash: point.Hash}, nil
}

func (e *Engine) historyCoverageCurrent(ctx context.Context, coverage HistoryCoverage, id chain.ID) bool {
	now := time.Now().Unix()
	point := e.archiveCurrent[id]
	if coverage.ID == "" || coverage.Hash == "" || point.Hash == "" || coverage.Height > point.Height || !e.fresh(id) || e.chainObserved[id] <= 0 || e.chainObserved[id] > now || now-e.chainObserved[id] > 120 || !e.activitySourceCurrent(id, point.Generation) {
		return false
	}
	source, ok := e.nodes[id].(recoveryBlockHasher)
	if !ok {
		return false
	}
	hash, err := source.BlockHash(ctx, coverage.Height)
	if err != nil || hash != coverage.Hash {
		return false
	}
	tip, err := source.BlockHash(ctx, point.Height)
	return err == nil && tip == point.Hash && e.activitySourceCurrent(id, point.Generation)
}

func activityCovered(a Activity, coverage HistoryCoverage) bool {
	if coverage.ID == "" || a.ArchiveCoverage != coverage.ID || a.Status != "confirmed" || a.TxID == "" || a.BlockHash == "" {
		return false
	}
	now := time.Now().Unix()
	for _, o := range a.Observations {
		if o.TxID == a.TxID && o.BlockHash == a.BlockHash && o.Status == "confirmed" && o.Confirmations >= archiveSettlementDepth && o.Height > 0 && o.Height <= coverage.Height && coverage.Height-o.Height+1 >= archiveSettlementDepth && o.ObservedAt > 0 && o.ObservedAt <= now {
			return true
		}
	}
	return false
}

// Validate optional advisory coverage without inventing it for older snapshots.
// The bound identifies history only; successful parsing is never chain proof.
func ValidateHistoryCoverage(s *State) error {
	if s == nil {
		return errors.New("missing history state")
	}
	validID := func(id string) bool { raw, err := hex.DecodeString(id); return err == nil && len(raw) == 32 }
	if s.Capacity != nil {
		for id, prefix := range s.Capacity.HistoryCoverage {
			if !id.Valid() || !validID(prefix.ID) || prefix.Height == 0 || prefix.Hash == "" || len(prefix.Hash) > 128 {
				return errors.New("invalid retained historical coverage")
			}
		}
	}
	for _, a := range s.Activities {
		if a.ArchiveCoverage != "" && !validID(a.ArchiveCoverage) {
			return errors.New("invalid historical row coverage binding")
		}
	}
	return nil
}
