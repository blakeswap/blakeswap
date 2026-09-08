package daemon

import (
	"errors"
	"slices"
	"strings"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/btcsuite/btcd/wire"
)

// FundingParent binds one selected wallet change output to the exact local
// child that created it. Edges follow signed input order; they never grant
// signing authority or require loading a transitive transaction graph.
type FundingParent struct {
	SwapID string   `json:"swap_id"`
	Chain  chain.ID `json:"chain"`
	TxID   string   `json:"txid"`
	Vout   uint32   `json:"vout"`
}

// A retained inclusion is evidence to check for contradiction, not a reusable
// authorization. Every positive execution/settlement proof is checked anew.
type FundingAnchor struct {
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
}

func localFunding(s *Swap) (contract.HTLC, string, bool) {
	if s.Role == "maker" {
		return s.Short, s.ShortFunding, s.ShortSent
	}
	return s.Long, s.LongFunding, s.LongSent
}

func localFundingTransaction(s *Swap) (*wire.MsgTx, error) {
	own, raw, _ := localFunding(s)
	if raw == "" {
		return nil, nil
	}
	tx, err := contract.Parse(raw)
	if err != nil || (own.Chain != chain.BTC && own.Chain != chain.Blake) || tx.TxHash().String() != own.TxID || uint64(own.Vout) >= uint64(len(tx.TxOut)) {
		return nil, errors.New("local funding bytes differ from their retained contract")
	}
	pk, err := own.PkScript()
	if err != nil || tx.TxOut[own.Vout].Value != own.Amount || !slices.Equal(pk, tx.TxOut[own.Vout].PkScript) {
		return nil, errors.New("local funding output differs from its agreed contract")
	}
	return tx, nil
}

func fundingIdentityKey(id chain.ID, txid string) string { return "funding/" + string(id) + "/" + txid }
func validFundingIdentityKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 3 && parts[0] == "funding" && (parts[1] == string(chain.BTC) || parts[1] == string(chain.Blake)) && protocol.Hex32(parts[2])
}

// Exact owned point reads join a bounded signed input set to its direct parent
// core. Unrelated wallet transactions have no such ownership key. Cold parents
// and their immutable keys remain cold; no execution/publication is promoted.
func deriveFundingParents(state *State, reader FillStateReader, s *Swap) ([]FundingParent, error) {
	tx, err := localFundingTransaction(s)
	if err != nil || tx == nil {
		return nil, err
	}
	own, _, _ := localFunding(s)
	var parents []FundingParent
	seen := map[wire.OutPoint]bool{}
	for _, in := range tx.TxIn {
		if in == nil || seen[in.PreviousOutPoint] {
			return nil, errors.New("local funding has duplicate or missing inputs")
		}
		seen[in.PreviousOutPoint] = true
		point := in.PreviousOutPoint
		owner, err := fillValue[string](state, reader, "fill_keys", fundingIdentityKey(own.Chain, point.Hash.String()))
		if err != nil {
			return nil, err
		}
		if owner == nil {
			continue
		}
		if !protocol.Hex32(*owner) || *owner == s.ID {
			return nil, errors.New("invalid funding ancestor identity")
		}
		parent, err := fillValue[Swap](state, reader, "swaps", *owner)
		if err != nil {
			return nil, err
		}
		if parent == nil || parent.ID != *owner {
			return nil, errors.New("funding ancestor core is unavailable")
		}
		funding, err := localFundingTransaction(parent)
		if err != nil {
			return nil, err
		}
		parentOwn, _, _ := localFunding(parent)
		if funding == nil || parentOwn.Chain != own.Chain || funding.TxHash() != point.Hash || uint64(point.Index) >= uint64(len(funding.TxOut)) || point.Index == parentOwn.Vout || funding.TxOut[point.Index].Value <= 0 {
			return nil, errors.New("selected funding ancestor is not its exact change output")
		}
		parents = append(parents, FundingParent{SwapID: *owner, Chain: own.Chain, TxID: point.Hash.String(), Vout: point.Index})
	}
	return parents, nil
}

func validateSwapFundingParents(state *State, reader FillStateReader, s *Swap) error {
	expected, err := deriveFundingParents(state, reader, s)
	if err != nil {
		return err
	}
	if !slices.Equal(expected, s.FundingParents) {
		return errors.New("funding ancestry differs from the exact selected change inputs")
	}
	if len(expected) == 0 && (s.FundingAncestryHeld || s.FundingAncestryAnchor != nil) {
		return errors.New("unrelated child has funding ancestry evidence")
	}
	if a := s.FundingAncestryAnchor; a != nil && (a.Height == 0 || a.Hash == "") {
		return errors.New("invalid funding ancestry checkpoint")
	}
	return nil
}

func (e *Engine) retainFundingParents(s *Swap) error {
	parents, err := deriveFundingParents(&e.s, engineFillReader{e}, s)
	if err != nil {
		return err
	}
	if len(s.FundingParents) > 0 && !slices.Equal(s.FundingParents, parents) {
		return errors.New("signed funding ancestry cannot change")
	}
	s.FundingParents = parents
	if len(parents) > 0 {
		s.FundingAncestryHeld = true
	}
	return nil
}
