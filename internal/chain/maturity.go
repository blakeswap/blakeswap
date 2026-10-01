package chain

import (
	"encoding/json"
	"errors"
)

// coinbaseMaturity follows Knots 29.4.2 mempool policy, which applies the
// longer maturity to ALL coinbases, even before/after the consensus window.
// Confirmations equal the age at the next (candidate spend) block.
func (n Network) coinbaseMaturity(id ID) int {
	if id == Blake {
		switch n.Normalized() {
		case Mainnet:
			return 6480
		case Testnet:
			return 6705
		}
	}
	return 100
}

// Public RPC endpoints must advertise the pinned long-maturity deployment.
// Regtest leaves this optional because upstream enables it only by override.
func checkLongCoinbaseDeployment(n Network, tip int, raw json.RawMessage) error {
	if n.Normalized() == Regtest {
		return nil
	}
	var deployments map[string]struct {
		Type     string `json:"type"`
		Height   int    `json:"height"`
		End      int    `json:"height_end"`
		Start    int    `json:"coinbase_start_height"`
		Maturity int    `json:"maturity"`
		Active   *bool  `json:"active"`
	}
	start, enforce, release := 973440, 973440, 979920
	if n == Testnet {
		start, enforce, release = 151406, 151550, 158111
	}
	err := json.Unmarshal(raw, &deployments)
	d := deployments["long_coinbase_maturity"]
	active := tip >= enforce-1 && tip < release-1
	if err != nil || d.Type != "flagday" || d.Height != enforce || d.End != release-1 || d.Start != start || d.Maturity != n.coinbaseMaturity(Blake) || d.Active == nil || *d.Active != active {
		return errors.New("Blake2b RPC requires the long_coinbase_maturity deployment from Knots 29.4.2.knots20260508; upgrade the node and finish chainstate revalidation")
	}
	return nil
}
