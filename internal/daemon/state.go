// Package daemon owns wallet keys, durable protocol state, and local user commands.
package daemon

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
)

type NodeConfig struct {
	Kind              string `json:"kind"`
	CertificateSHA256 string `json:"certificate_sha256"`
	URL               string `json:"url"`
	Cookie            string `json:"cookie"`
}
type TowerConfig = protocol.Tower
type Config struct {
	RescueFeeBPS        int64                   `json:"rescue_fee_bps,omitempty"`
	ChainReady          func(chain.ID, uint32)  `json:"-"`
	PublicWatchtower    bool                    `json:"public_watchtower"`
	FavoriteWatchtowers []string                `json:"favorite_watchtowers,omitempty"`
	InitialMnemonic     string                  `json:"-"`
	Network             chain.Network           `json:"network"`
	Name                string                  `json:"name"`
	Mode                string                  `json:"mode"`
	DataDir             string                  `json:"data_dir"`
	PasswordFile        string                  `json:"password_file"`
	Socket              string                  `json:"socket"`
	Relays              []string                `json:"relays"`
	Nodes               map[chain.ID]NodeConfig `json:"nodes"`
	Tower               TowerConfig             `json:"tower"`
}
type Delivery struct {
	Expires     int64       `json:"expires,omitempty"`
	Type        string      `json:"type,omitempty"`
	Event       nostr.Event `json:"event"`
	To          string      `json:"to"`
	MessageID   string      `json:"message_id"`
	Digest      string      `json:"digest"`
	IsAck       bool        `json:"is_ack"`
	LastAttempt int64       `json:"last_attempt"`
	Published   bool        `json:"published"`
}
type Swap struct {
	ID                 string                      `json:"id"`
	Role               string                      `json:"role"`
	Request            protocol.Request            `json:"request"`
	Terms              *protocol.Terms             `json:"terms,omitempty"`
	Secret             string                      `json:"secret,omitempty"`
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
	CoinReservations map[string]CoinReservation `json:"coin_reservations,omitempty"`
	Sends            map[string]*WalletSend     `json:"sends,omitempty"`
	ReceiveIndexes   map[chain.ID]uint32        `json:"receive_indexes,omitempty"`
	DiscoverySeen    map[string]int64           `json:"discovery_seen,omitempty"`
	TowerPublic      bool                       `json:"tower_public,omitempty"`
	Towers           map[string]nostr.Event     `json:"towers,omitempty"`
	Network          chain.Network              `json:"network,omitempty"`
	Version          int                        `json:"version"`
	Mnemonic         string                     `json:"mnemonic"`
	Paused           bool                       `json:"paused"`
	Offers           map[string]nostr.Event     `json:"offers"`
	Book             map[string]nostr.Event     `json:"book"`
	Swaps            map[string]*Swap           `json:"swaps"`
	Outbox           map[string]*Delivery       `json:"outbox"`
	Seen             map[string]string          `json:"seen"`
	TowerJobs        map[string]*TowerJob       `json:"tower_jobs"`
	EventTime        nostr.Timestamp            `json:"event_time"`
}
type PublicSwap struct {
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
type Status struct {
	Coins           []PublicCoin        `json:"coins"`
	Sends           []PublicSend        `json:"sends"`
	OwnWatchtower   protocol.Tower      `json:"own_watchtower"`
	Watchtowers     []protocol.Tower    `json:"watchtowers"`
	FundingFee      int64               `json:"funding_fee"`
	Network         chain.Network       `json:"network"`
	Name            string              `json:"name"`
	Mode            string              `json:"mode"`
	PubKey          string              `json:"pubkey"`
	Addresses       map[chain.ID]string `json:"addresses"`
	Balances        map[chain.ID]int64  `json:"balances"`
	Heights         map[chain.ID]uint32 `json:"heights"`
	Paused          bool                `json:"paused"`
	Orders          []protocol.Offer    `json:"orders"`
	Swaps           []PublicSwap        `json:"swaps"`
	TowerJobs       []map[string]any    `json:"tower_jobs"`
	PendingMessages int                 `json:"pending_messages"`
	LastError       string              `json:"last_error"`
	Tower           TowerConfig         `json:"tower"`
}
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type Response struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
