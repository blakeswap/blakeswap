package daemon

import "github.com/blakeswap/blakeswap/internal/chain"

// Activity is a safe projection of encrypted recovery state. It deliberately has
// no raw transaction, preimage, private key, credential or protocol-message field.
// Amounts are native satoshis. Movement=false identifies a related informational
// record (such as change), which must not be counted as another payment.
type ActivityVariant struct {
	TxID      string `json:"txid"`
	Amount    int64  `json:"amount"`
	Principal int64  `json:"principal"`
	Fee       int64  `json:"fee"`
	FeeKnown  bool   `json:"fee_known"`
	FeePayer  string `json:"fee_payer"`
	Bounty    int64  `json:"bounty"`
}

type Activity struct {
	Version        int                   `json:"version"`
	ID             string                `json:"id"`
	GroupID        string                `json:"group_id"`
	Wallet         string                `json:"wallet"`
	Network        chain.Network         `json:"network"`
	Kind           string                `json:"kind"`
	Chain          chain.ID              `json:"chain"`
	Direction      string                `json:"direction"`
	Movement       bool                  `json:"movement"`
	Amount         int64                 `json:"amount"`
	Principal      int64                 `json:"principal"`
	Fee            int64                 `json:"fee"`
	FeeKnown       bool                  `json:"fee_known"`
	FeePayer       string                `json:"fee_payer"`
	Bounty         int64                 `json:"bounty"`
	CounterChain   chain.ID              `json:"counter_chain"`
	CounterAmount  int64                 `json:"counter_amount"`
	OrderID        string                `json:"order_id"`
	SwapID         string                `json:"swap_id"`
	SendID         string                `json:"send_id"`
	RelatedIDs     []string              `json:"related_ids"`
	Address        string                `json:"address"`
	Outpoints      []CoinOutpoint        `json:"outpoints"`
	TxID           string                `json:"txid"`
	Variants       []string              `json:"variants"`
	LocalStatus    string                `json:"local_status"`
	Status         string                `json:"status"`
	Confirmations  int                   `json:"confirmations"`
	CreatedAt      int64                 `json:"created_at"`
	CreatedSource  string                `json:"created_source"`
	RecordedAt     int64                 `json:"recorded_at"`
	UpdatedAt      int64                 `json:"updated_at"`
	BlockTime      int64                 `json:"block_time"`
	BlockHash      string                `json:"block_hash"`
	Label          string                `json:"label"`
	Source         string                `json:"source"`
	Generation     uint64                `json:"generation"`
	ObservedAt     int64                 `json:"observed_at"`
	VariantAmounts []ActivityVariant     `json:"variant_amounts"`
	Observations   []ActivityObservation `json:"observations"`
	History        []ActivityOutcome     `json:"history"`
}
type ActivityObservation struct {
	Sequence      uint64 `json:"sequence"`
	TxID          string `json:"txid"`
	Status        string `json:"status"`
	Confirmations int    `json:"confirmations"`
	Height        uint32 `json:"height"`
	BlockHash     string `json:"block_hash"`
	BlockTime     int64  `json:"block_time"`
	ObservedAt    int64  `json:"observed_at"`
	Source        string `json:"source"`
	Generation    uint64 `json:"generation"`
	Error         string `json:"error"`
}
type ActivityOutcome struct {
	Status     string `json:"status"`
	TxID       string `json:"txid"`
	Amount     int64  `json:"amount"`
	Fee        int64  `json:"fee"`
	FeeKnown   bool   `json:"fee_known"`
	BlockHash  string `json:"block_hash"`
	BlockTime  int64  `json:"block_time"`
	ObservedAt int64  `json:"observed_at"`
	Source     string `json:"source"`
	Generation uint64 `json:"generation"`
}
type ActivityIndex struct {
	Address       uint32 `json:"address"`
	After         string `json:"after"`
	Source        string `json:"source"`
	Generation    uint64 `json:"generation"`
	CompletedPass int64  `json:"completed_pass"`
	Error         string `json:"error"`
}
type ActivityQuery struct {
	ExpectedWallet  string   `json:"expected_wallet"`
	ExpectedNetwork string   `json:"expected_network"`
	Kind            string   `json:"kind"`
	Status          string   `json:"status"`
	Chain           chain.ID `json:"chain"`
	From            int64    `json:"from"`
	To              int64    `json:"to"`
	Snapshot        string   `json:"snapshot"`
	Cursor          uint32   `json:"cursor"`
	Limit           uint32   `json:"limit"`
}
type ActivityPage struct {
	Snapshot   string                     `json:"snapshot"`
	Expires    int64                      `json:"expires"`
	Revision   uint64                     `json:"revision"`
	Total      uint32                     `json:"total"`
	NextCursor uint32                     `json:"next_cursor"`
	Records    []Activity                 `json:"records"`
	Index      map[chain.ID]ActivityIndex `json:"index"`
	Error      string                     `json:"error"`
}
type ActivityExport struct {
	Snapshot   string `json:"snapshot"`
	Expires    int64  `json:"expires"`
	NextCursor uint32 `json:"next_cursor"`
	Total      uint32 `json:"total"`
	CSV        string `json:"csv"`
}
type activitySnapshot struct {
	Page   ActivityPage
	Filter string
}
