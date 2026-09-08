// Package daemon owns wallet keys, durable protocol state, and local user commands.
package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/credential"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
)

type NodeConfig = chain.Endpoint
type TowerConfig = protocol.Tower
type Config struct {
	CredentialMode      string                   `json:"credential_mode,omitempty"`
	Credential          credential.Source        `json:"-"`
	Authorization       *authorization.Authority `json:"-"`
	Installation        string                   `json:"-"`
	RescueFeeBPS        int64                    `json:"rescue_fee_bps,omitempty"`
	ChainReady          func(chain.ID, uint32)   `json:"-"`
	PublicWatchtower    bool                     `json:"public_watchtower"`
	FavoriteWatchtowers []string                 `json:"favorite_watchtowers,omitempty"`
	InitialMnemonic     string                   `json:"-"`
	Network             chain.Network            `json:"network"`
	Name                string                   `json:"name"`
	Mode                string                   `json:"mode"`
	DataDir             string                   `json:"data_dir"`
	PasswordFile        string                   `json:"password_file"`
	Socket              string                   `json:"socket"`
	Relays              []string                 `json:"relays"`
	Nodes               map[chain.ID]NodeConfig  `json:"nodes"`
	Tower               TowerConfig              `json:"tower"`
}
type Delivery struct {
	Version      int           `json:"version"`
	Network      chain.Network `json:"network"`
	SwapID       string        `json:"swap_id,omitempty"`
	Acknowledged bool          `json:"acknowledged,omitempty"`
	Expires      int64         `json:"expires,omitempty"`
	Type         string        `json:"type,omitempty"`
	Event        nostr.Event   `json:"event"`
	To           string        `json:"to"`
	MessageID    string        `json:"message_id"`
	Digest       string        `json:"digest"`
	IsAck        bool          `json:"is_ack"`
	LastAttempt  int64         `json:"last_attempt"`
	Published    bool          `json:"published"`
}
type Swap struct {
	ClaimVariant       int                         `json:"claim_variant,omitempty"`
	RefundVariant      int                         `json:"refund_variant,omitempty"`
	OwnerFeeCap        int64                       `json:"owner_fee_cap,omitempty"`
	SelfClaims         []string                    `json:"self_claims,omitempty"`
	ClaimAttempt       int                         `json:"claim_attempt,omitempty"`
	RefundAttempt      int                         `json:"refund_attempt,omitempty"`
	ClaimLastAttempt   int64                       `json:"claim_last_attempt,omitempty"`
	RefundLastAttempt  int64                       `json:"refund_last_attempt,omitempty"`
	Protection         *protocol.Tower             `json:"protection,omitempty"`
	ID                 string                      `json:"id"`
	Role               string                      `json:"role"`
	Request            protocol.Request            `json:"request"`
	Terms              *protocol.Terms             `json:"terms,omitempty"`
	Secret             string                      `json:"secret,omitempty"`
	SecretObserved     bool                        `json:"secret_observed"`
	IncomingClaimSeen  bool                        `json:"incoming_claim_seen"`
	SecretExposed      bool                        `json:"secret_exposed"`
	Long               contract.HTLC               `json:"long"`
	Short              contract.HTLC               `json:"short"`
	LongFunding        string                      `json:"long_funding,omitempty"`
	ShortFunding       string                      `json:"short_funding,omitempty"`
	LongSent           bool                        `json:"long_sent"`
	ShortSent          bool                        `json:"short_sent"`
	SelfRefunds        []string                    `json:"self_refunds,omitempty"`
	SelfClaim          string                      `json:"self_claim,omitempty"`
	Jobs               []protocol.Job              `json:"jobs,omitempty"`
	Receipts           map[string]protocol.Receipt `json:"receipts"`
	Stage              string                      `json:"stage"`
	Error              string                      `json:"error,omitempty"`
	LongSpend          string                      `json:"long_spend,omitempty"`
	ShortSpend         string                      `json:"short_spend,omitempty"`
	LongConfirmations  int                         `json:"long_confirmations"`
	ShortConfirmations int                         `json:"short_confirmations"`
	TowerPaid          int64                       `json:"tower_paid"`
	TowerPayments      map[chain.ID]int64          `json:"tower_payments"`
}
type TowerJob struct {
	Variants    []string     `json:"variants,omitempty"`
	FundingSeen bool         `json:"funding_seen,omitempty"`
	Expired     bool         `json:"expired,omitempty"`
	Job         protocol.Job `json:"job"`
	Secret      string       `json:"secret,omitempty"`
	Broadcast   string       `json:"broadcast,omitempty"`
	Confirmed   int          `json:"confirmed"`
	LastAttempt int64        `json:"last_attempt"`
	Attempt     int          `json:"attempt"`
	Error       string       `json:"error,omitempty"`
}
type State struct {
	ParentOrders                map[string]*ParentOrder      `json:"parent_orders,omitempty"`
	FillRecords                 map[string]*FillRecord       `json:"fill_records,omitempty"`
	FillKeys                    map[string]string            `json:"fill_keys,omitempty"`
	MakerStrategies             map[string]*MakerStrategy    `json:"maker_strategies,omitempty"`
	OwnPublicVersions           map[string]PublicVersion     `json:"own_public_versions,omitempty"`
	PublicVersions              map[string]PublicVersion     `json:"public_versions,omitempty"`
	PublicLimited               bool                         `json:"public_limited,omitempty"`
	ActivityTransactions        map[string]string            `json:"activity_transactions,omitempty"`
	ActivityOwned               map[string]int64             `json:"activity_owned,omitempty"`
	RelaySync                   map[string]RelaySyncRecord   `json:"relay_sync,omitempty"`
	Automations                 map[string]*AutomationPolicy `json:"automations,omitempty"`
	SeenSemantics               map[string]bool              `json:"seen_semantics,omitempty"`
	TradeTokens                 map[string]string            `json:"trade_tokens,omitempty"`
	Capacity                    *CapacityRecord              `json:"capacity,omitempty"`
	Archive                     []storage.ArchiveRecord      `json:"archive,omitempty"`
	OrderRecords                map[string]OrderRecord       `json:"order_records,omitempty"`
	Recovery                    *RecoveryRecord              `json:"recovery,omitempty"`
	Backup                      *BackupRecord                `json:"backup,omitempty"`
	ActivityReceipts            map[string]ReceiptEvidence   `json:"activity_receipts"`
	ActivityObservationSequence uint64                       `json:"activity_observation_sequence"`
	ActivityVersion             int                          `json:"activity_version"`
	Activities                  map[string]Activity          `json:"activities"`
	ActivityRevision            uint64                       `json:"activity_revision"`
	ActivityIndexes             map[chain.ID]ActivityIndex   `json:"activity_indexes"`
	ActivityError               string                       `json:"activity_error"`
	TradeReceipts               map[string]*TradeReceipt     `json:"trade_receipts,omitempty"`
	FundingFees                 map[string]FeeSelection      `json:"funding_fees,omitempty"`
	OfferTowers                 map[string]protocol.Tower    `json:"offer_towers,omitempty"`
	CoinReservations            map[string]CoinReservation   `json:"coin_reservations,omitempty"`
	Sends                       map[string]*WalletSend       `json:"sends,omitempty"`
	ReceiveIndexes              map[chain.ID]uint32          `json:"receive_indexes,omitempty"`
	DiscoverySeen               map[string]int64             `json:"discovery_seen,omitempty"`
	TowerPublic                 bool                         `json:"tower_public,omitempty"`
	Towers                      map[string]nostr.Event       `json:"towers,omitempty"`
	Network                     chain.Network                `json:"network,omitempty"`
	Version                     int                          `json:"version"`
	Mnemonic                    string                       `json:"mnemonic"`
	Paused                      bool                         `json:"paused"`
	Offers                      map[string]nostr.Event       `json:"offers"`
	Book                        map[string]nostr.Event       `json:"book"`
	Swaps                       map[string]*Swap             `json:"swaps"`
	Outbox                      map[string]*Delivery         `json:"outbox"`
	Seen                        map[string]string            `json:"seen"`
	TowerJobs                   map[string]*TowerJob         `json:"tower_jobs"`
	EventTime                   nostr.Timestamp              `json:"event_time"`
}
type PublicSwap struct {
	ParentID           string             `json:"parent_id"`
	ParentMaker        string             `json:"parent_maker"`
	ParentRevision     uint64             `json:"parent_revision"`
	Quantity           int64              `json:"quantity"`
	Allocation         FillDisposition    `json:"allocation"`
	AllocatedQuantity  int64              `json:"allocated_quantity"`
	AllocationKnown    bool               `json:"allocation_known"`
	ClaimFee           int64              `json:"claim_fee"`
	RefundFee          int64              `json:"refund_fee"`
	ClaimTxID          string             `json:"claim_txid"`
	RefundTxID         string             `json:"refund_txid"`
	OwnerFeeCap        int64              `json:"owner_fee_cap"`
	FundingFee         int64              `json:"funding_fee"`
	ClaimVariants      []string           `json:"claim_variants"`
	RefundVariants     []string           `json:"refund_variants"`
	ID                 string             `json:"id"`
	Role               string             `json:"role"`
	Stage              string             `json:"stage"`
	Error              string             `json:"error,omitempty"`
	Long               contract.HTLC      `json:"long"`
	Short              contract.HTLC      `json:"short"`
	LongSpend          string             `json:"long_spend,omitempty"`
	ShortSpend         string             `json:"short_spend,omitempty"`
	LongConfirmations  int                `json:"long_confirmations"`
	ShortConfirmations int                `json:"short_confirmations"`
	TowerPaid          int64              `json:"tower_paid"`
	TowerPayments      map[chain.ID]int64 `json:"tower_payments"`
	TowerReady         bool               `json:"tower_ready"`
	TowerEnabled       bool               `json:"tower_enabled"`
	SecretRevealed     bool               `json:"secret_revealed"`
	Takeover           uint32             `json:"takeover"`
	RevealBefore       uint32             `json:"reveal_before"`
}
type ChainConnection struct {
	Ready           bool                 `json:"ready"`
	LastObservation int64                `json:"last_observation"`
	Error           string               `json:"error"`
	Sources         chain.EndpointStatus `json:"sources"`
}
type Status struct {
	Actions         WalletActions                `json:"actions"`
	Capacity        CapacityHealth               `json:"capacity"`
	Backup          BackupFreshness              `json:"backup"`
	Recovery        *RecoveryStatus              `json:"recovery,omitempty"`
	FeeLimits       map[chain.ID]FeeLimits       `json:"fee_limits"`
	Connections     map[chain.ID]ChainConnection `json:"connections"`
	Funds           map[chain.ID]ChainBalance    `json:"funds"`
	Coins           []PublicCoin                 `json:"coins"`
	Sends           []PublicSend                 `json:"sends"`
	OwnWatchtower   protocol.Tower               `json:"own_watchtower"`
	Watchtowers     []protocol.Tower             `json:"watchtowers"`
	FundingFee      int64                        `json:"funding_fee"`
	Network         chain.Network                `json:"network"`
	Name            string                       `json:"name"`
	Mode            string                       `json:"mode"`
	PubKey          string                       `json:"pubkey"`
	Addresses       map[chain.ID]string          `json:"addresses"`
	Balances        map[chain.ID]int64           `json:"balances"`
	Heights         map[chain.ID]uint32          `json:"heights"`
	Paused          bool                         `json:"paused"`
	Orders          []protocol.Offer             `json:"orders"`
	Swaps           []PublicSwap                 `json:"swaps"`
	TowerJobs       []map[string]any             `json:"tower_jobs"`
	PendingMessages int                          `json:"pending_messages"`
	LastError       string                       `json:"last_error"`
	Tower           TowerConfig                  `json:"tower"`
}
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type Response struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
