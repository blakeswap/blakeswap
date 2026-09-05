// Package chain provides full-node and Electrum chain observations.
package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type ID string

const (
	BTC   ID = "btc"
	Blake ID = "blake"
)

func (id ID) Valid() bool { return id == BTC || id == Blake }
func (id ID) Other() ID {
	if id == BTC {
		return Blake
	}
	return BTC
}

// IDs include the rule set: these two regtest networks share their genesis hash.
func (id ID) Domain() string {
	if id == BTC {
		return "bitcoin:regtest:bip143"
	}
	return "bitcoin-blake2b:regtest:activation1:unified21"
}

type RPC struct {
	ID      ID
	Network Network
	URL     string
	Cookie  string
	Wallet  string
	client  *http.Client
}

func New(id ID, endpoint, cookie string) (*RPC, error) {
	return NewFor(Regtest, id, endpoint, cookie)
}
func NewFor(network Network, id ID, endpoint, cookie string) (*RPC, error) {
	u, e := url.Parse(endpoint)
	if e != nil {
		return nil, e
	}
	if !id.Valid() || !network.Valid() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1"))) {
		return nil, errors.New("RPC requires HTTPS or explicit loopback HTTP; credentials belong in a cookie file")
	}
	return &RPC{ID: id, Network: network.Normalized(), URL: strings.TrimRight(endpoint, "/"), Cookie: cookie, client: &http.Client{Timeout: 15 * time.Second}}, nil
}
func (r *RPC) WithWallet(name string) *RPC { c := *r; c.Wallet = name; return &c }

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }
func (r *RPC) Call(ctx context.Context, method string, out any, params ...any) error {
	return r.call(ctx, r.client, method, out, params...)
}

// Wallet history operations can take hours. Their caller owns cancellation;
// ordinary observations retain the short HTTP timeout.
func (r *RPC) historyCall(ctx context.Context, method string, out any, params ...any) error {
	client := *r.client
	client.Timeout = 0
	return r.call(ctx, &client, method, out, params...)
}

func (r *RPC) call(ctx context.Context, client *http.Client, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	endpoint := r.URL
	if r.Wallet != "" {
		endpoint += "/wallet/" + url.PathEscape(r.Wallet)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	cookie, err := os.ReadFile(r.Cookie)
	if err != nil {
		return err
	}
	user, pass, ok := strings.Cut(strings.TrimSpace(string(cookie)), ":")
	if !ok {
		return errors.New("invalid RPC cookie")
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err = json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("RPC %s HTTP %d: invalid response", method, resp.StatusCode)
	}
	if reply.Error != nil {
		return reply.Error
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("RPC HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}
func (r *RPC) Check(ctx context.Context) error {
	var info struct {
		Chain         string
		Blocks        int
		BestBlockHash string
	}
	if e := r.Call(ctx, "getblockchaininfo", &info); e != nil {
		return e
	}
	if info.Chain != r.Network.NodeName() || info.Blocks < int(r.Network.ForkHeight())+1 {
		return errors.New("wrong network or chain has not passed Blake2b activation")
	}
	var genesis string
	if err := r.Call(ctx, "getblockhash", &genesis, 0); err != nil {
		return err
	}
	if genesis != r.Network.Genesis() {
		return errors.New("wrong chain genesis")
	}
	var header string
	if e := r.Call(ctx, "getblockheader", &header, info.BestBlockHash, false); e != nil {
		return e
	}
	expected := 160
	if r.ID == Blake {
		expected = 328
		var dep map[string]json.RawMessage
		if e := r.Call(ctx, "getdeploymentinfo", &dep); e != nil {
			return e
		}
		var fork struct {
			Active bool
			Height int
		}
		if e := json.Unmarshal(dep["blake2b"], &fork); e != nil {
			return e
		}
		if r.Network == Mainnet {
			var hash string
			if err := r.Call(ctx, "getblockhash", &hash, r.Network.ForkHeight()); err != nil {
				return err
			}
			if hash != "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb" {
				return errors.New("Blake2b checkpoint mismatch")
			}
		}
		if !fork.Active || fork.Height != int(r.Network.ForkHeight()) {
			return errors.New("wrong Blake2b activation")
		}
	}
	if len(header) != expected {
		return fmt.Errorf("chain identity mismatch: %s header has %d bytes, expected %d", r.ID, len(header)/2, expected/2)
	}
	return nil
}
func (r *RPC) Height(ctx context.Context) (uint32, error) {
	var h uint32
	e := r.Call(ctx, "getblockcount", &h)
	return h, e
}
func (r *RPC) Broadcast(ctx context.Context, raw string) (string, error) {
	var id string
	e := r.Call(ctx, "sendrawtransaction", &id, raw)
	return id, e
}

// Coins preserves all 8 decimal places without binary floating point.
type Coins int64

func (c *Coins) UnmarshalJSON(raw []byte) error {
	s := string(raw)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 8 {
		return errors.New("fractional satoshi")
	}
	frac += strings.Repeat("0", 8-len(frac))
	var n int64
	for _, ch := range whole + frac {
		if ch < '0' || ch > '9' {
			return errors.New("invalid coin amount")
		}
		if n > 2100000000000000/10 {
			return errors.New("amount overflow")
		}
		n = n*10 + int64(ch-'0')
	}
	if n > 2100000000000000 {
		return errors.New("amount exceeds money range")
	}
	if neg {
		n = -n
	}
	*c = Coins(n)
	return nil
}
func (c Coins) MarshalJSON() ([]byte, error) {
	if c < 0 {
		return []byte(fmt.Sprintf("-%d.%08d", -c/100000000, -c%100000000)), nil
	}
	return []byte(fmt.Sprintf("%d.%08d", c/100000000, c%100000000)), nil
}

type UTXO struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	Amount        Coins  `json:"amount"`
	Script        string `json:"scriptPubKey"`
	Confirmations int    `json:"confirmations"`
}
type TxOut struct {
	Value         Coins `json:"value"`
	Confirmations int   `json:"confirmations"`
	Script        struct {
		Hex string `json:"hex"`
	} `json:"scriptPubKey"`
}

func (r *RPC) Output(ctx context.Context, txid string, vout uint32) (*TxOut, error) {
	var o *TxOut
	e := r.Call(ctx, "gettxout", &o, txid, vout, true)
	return o, e
}

type Transaction struct {
	Height        uint32 `json:"height"`
	Hex           string `json:"hex"`
	TxID          string `json:"txid"`
	Confirmations int    `json:"confirmations"`
	BlockHash     string `json:"blockhash"`
}

func (r *RPC) Transaction(ctx context.Context, id string) (Transaction, error) {
	var t Transaction
	e := r.Call(ctx, "getrawtransaction", &t, id, true)
	if e == nil && t.BlockHash != "" {
		var h struct{ Height uint32 }
		if err := r.Call(ctx, "getblockheader", &h, t.BlockHash); err != nil {
			return t, err
		}
		t.Height = h.Height
	}
	return t, e
}

// Observe imports addresses into a watch-only node wallet. No spending key crosses RPC.
func (r *RPC) Observe(ctx context.Context, name string, addresses []string) (Backend, error) {
	w := r.WithWallet(name)
	var loaded []string
	if e := r.Call(ctx, "listwallets", &loaded); e != nil {
		return nil, e
	}
	found := false
	for _, n := range loaded {
		if n == name {
			found = true
		}
	}
	if !found {
		var dirs struct{ Wallets []struct{ Name string } }
		if e := r.Call(ctx, "listwalletdir", &dirs); e != nil {
			return nil, e
		}
		exists := false
		for _, n := range dirs.Wallets {
			if n.Name == name {
				exists = true
			}
		}
		var e error
		if exists {
			e = r.historyCall(ctx, "loadwallet", nil, name)
		} else {
			e = r.Call(ctx, "createwallet", nil, name, true, true, "", false, true)
		}
		if e != nil {
			return nil, e
		}
	}
	var info struct{ Scanning json.RawMessage }
	if err := w.Call(ctx, "getwalletinfo", &info); err != nil {
		return nil, err
	}
	if string(info.Scanning) != "false" {
		return nil, errors.New("RPC wallet history is synchronizing; waiting for the node rescan")
	}
	var descriptors struct {
		Descriptors []struct {
			Desc      string
			Timestamp int64
		}
	}
	if err := w.Call(ctx, "listdescriptors", &descriptors); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, d := range descriptors.Descriptors {
		known[d.Desc] = d.Timestamp >= 0 && d.Timestamp <= 1 // Core normalizes timestamp zero to one.
	}
	// Descriptor presence alone cannot prove that its initial rescan succeeded.
	// Record readiness in the same node wallet only after the complete response.
	const readyLabel = "blakeswap-history-ready-v1"
	var imports []any
	var pending []string
	for _, addr := range addresses {
		var d struct{ Descriptor string }
		if e := r.Call(ctx, "getdescriptorinfo", &d, "addr("+addr+")"); e != nil {
			return nil, e
		}
		var address struct{ Labels []string }
		if err := w.Call(ctx, "getaddressinfo", &address, addr); err != nil {
			return nil, err
		}
		ready := false
		for _, label := range address.Labels {
			ready = ready || label == readyLabel
		}
		if known[d.Descriptor] && ready {
			continue
		}
		imports = append(imports, map[string]any{"desc": d.Descriptor, "timestamp": 0})
		pending = append(pending, addr)
	}
	if len(imports) == 0 {
		return w, nil
	}
	var result []struct {
		Success bool
		Error   *RPCError
	}
	if e := w.historyCall(ctx, "importdescriptors", &result, imports); e != nil {
		return nil, e
	}
	if len(result) != len(imports) {
		return nil, errors.New("incomplete descriptor import response")
	}
	for _, r := range result {
		if !r.Success {
			return nil, fmt.Errorf("import failed: %v", r.Error)
		}
	}
	for _, addr := range pending {
		if err := w.Call(ctx, "setlabel", nil, addr, readyLabel); err != nil {
			return nil, err
		}
	}
	return w, nil
}
func (r *RPC) Unspent(ctx context.Context, addresses []string) ([]UTXO, error) {
	var list []UTXO
	e := r.Call(ctx, "listunspent", &list, 1, 9999999, addresses)
	return list, e
}
