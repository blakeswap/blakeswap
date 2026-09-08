package daemon

import (
	"sort"

	"github.com/blakeswap/blakeswap/internal/chain"
)

// Keep unrelated swaps in their ordinary sorted positions. Within each local
// funding chain, rotate dependent children through their existing slots so slow
// unknown funding cannot consume the shared read budget before the same suffix
// on every tick. This changes no evidence, timeout, or publication authority.
func (e *Engine) swapTickIDs() []string {
	ids := make([]string, 0, len(e.s.Swaps))
	for id := range e.s.Swaps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if e.fundingAncestryLastTurn == nil {
		e.fundingAncestryLastTurn = make(map[chain.ID]string)
	}
	for _, chainID := range []chain.ID{chain.BTC, chain.Blake} {
		var positions []int
		var children []string
		for i, id := range ids {
			s := e.s.Swaps[id]
			own, _, _ := localFunding(s)
			if len(s.FundingParents) > 0 && own.Chain == chainID {
				positions = append(positions, i)
				children = append(children, id)
			}
		}
		if len(children) == 0 {
			delete(e.fundingAncestryLastTurn, chainID)
			continue
		}
		last := e.fundingAncestryLastTurn[chainID]
		start := sort.Search(len(children), func(i int) bool { return children[i] > last }) % len(children)
		// Advance before execution: an unavailable child consumes its turn too.
		// Looking up the successor also handles deletion or archival of last.
		e.fundingAncestryLastTurn[chainID] = children[start]
		for i, position := range positions {
			ids[position] = children[(start+i)%len(children)]
		}
	}
	return ids
}
