package daemon

import (
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

// FillOrderFields is explicit create/replace/recreate authorization. Limits are
// integer units of each named asset, never a count or an unlimited zero default.
// These private budget choices do not enter the public signed offer.
type FillOrderFields struct {
	protocol.FillPolicy
	FeeBudgets    map[chain.ID]int64 `json:"fee_budgets"`
	BountyBudgets map[chain.ID]int64 `json:"bounty_budgets"`
}

// FillTakeFields binds the reviewed child to one exact signed parent revision.
// Quantity always uses the maker's sell asset; the buy quantity is derived.
type FillTakeFields struct {
	Quantity       int64  `json:"quantity"`
	ParentRevision uint64 `json:"parent_revision"`
}

// FillPreview is a separately labelled representative child for a partial
// creation review. Its outcomes must not be displayed as aggregate parent
// outcomes or substituted for a taker's explicitly requested quantity.
type FillPreview struct {
	Quantity    int64          `json:"quantity"`
	BuyAmount   int64          `json:"buy_amount"`
	FundingFee  int64          `json:"funding_fee"`
	OwnerFeeCap int64          `json:"owner_fee_cap"`
	Outcomes    []TradeOutcome `json:"outcomes"`
}

// QuantitySummary is a projection of the maker's conserved ledger. It excludes
// implementation accounting such as Withdrawn, which is already in Released.
// A foreign parent has no locally authoritative summary and returns nil.
type QuantitySummary struct {
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	Reserved  int64 `json:"reserved"`
	Committed int64 `json:"committed"`
	Filled    int64 `json:"filled"`
	Released  int64 `json:"released"`
}

// FillQuery is local retained history, scoped by wallet, network and both parts
// of the parent identity. Revision freezes the page set; empty starts a query.
// The implementation uses the existing bounded encrypted query lifecycle.
type FillQuery struct {
	ExpectedWallet  string `json:"expected_wallet"`
	ExpectedNetwork string `json:"expected_network"`
	ParentMaker     string `json:"parent_maker"`
	ParentID        string `json:"parent_id"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
	Revision        string `json:"revision"`
}

type FillSummary struct {
	ID                 string          `json:"id"`
	ParentMaker        string          `json:"parent_maker"`
	ParentID           string          `json:"parent_id"`
	ParentRevision     uint64          `json:"parent_revision"`
	Quantity           int64           `json:"quantity"`
	BuyAmount          int64           `json:"buy_amount"`
	Disposition        FillDisposition `json:"disposition"`
	AllocatedQuantity  int64           `json:"allocated_quantity"`
	AllocationKnown    bool            `json:"allocation_known"`
	Stage              string          `json:"stage"`
	Archived           bool            `json:"archived"`
	MonitoringRequired bool            `json:"monitoring_required"`
}

type FillPage struct {
	Wallet      string        `json:"wallet"`
	Network     chain.Network `json:"network"`
	ParentMaker string        `json:"parent_maker"`
	ParentID    string        `json:"parent_id"`
	Records     []FillSummary `json:"records"`
	Revision    string        `json:"revision"`
	Total       int           `json:"total"`
	NextOffset  int           `json:"next_offset"`
	More        bool          `json:"more"`
}
