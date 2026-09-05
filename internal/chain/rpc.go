// Package chain talks only to explicitly configured local regtest full nodes.
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
	ID     ID
	URL    string
	Cookie string
	Wallet string
	client *http.Client
}

func New(id ID, endpoint, cookie string) (*RPC, error) {
	u, e := url.Parse(endpoint)
	if e != nil {
		return nil, e
	}
	if !id.Valid() || u.Scheme != "http" || u.User != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return nil, errors.New("only explicit loopback regtest RPC endpoints are supported")
	}
	return &RPC{ID: id, URL: strings.TrimRight(endpoint, "/"), Cookie: cookie, client: &http.Client{Timeout: 15 * time.Second}}, nil
}
func (r *RPC) WithWallet(name string) *RPC { c := *r; c.Wallet = name; return &c }

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }
func (r *RPC) Call(ctx context.Context, method string, out any, params ...any) error {
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
	resp, err := r.client.Do(req)
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
	if info.Chain != "regtest" || info.Blocks < 2 {
		return errors.New("refusing non-regtest or uninitialized chain")
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
		if !fork.Active || fork.Height != 1 {
			return errors.New("wrong Blake2b regtest activation")
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
	Hex           string `json:"hex"`
	TxID          string `json:"txid"`
	Confirmations int    `json:"confirmations"`
	BlockHash     string `json:"blockhash"`
}

func (r *RPC) Transaction(ctx context.Context, id string) (Transaction, error) {
	var t Transaction
	e := r.Call(ctx, "getrawtransaction", &t, id, true)
	return t, e
}

// Observe imports addresses into a watch-only node wallet. No spending key crosses RPC.
func (r *RPC) Observe(ctx context.Context, name string, addresses []string) (*RPC, error) {
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
			e = r.Call(ctx, "loadwallet", nil, name)
		} else {
			e = r.Call(ctx, "createwallet", nil, name, true, true, "", false, true)
		}
		if e != nil {
			return nil, e
		}
	}
	var imports []any
	for _, addr := range addresses {
		var d struct{ Descriptor string }
		if e := r.Call(ctx, "getdescriptorinfo", &d, "addr("+addr+")"); e != nil {
			return nil, e
		}
		imports = append(imports, map[string]any{"desc": d.Descriptor, "timestamp": 0})
	}
	var result []struct {
		Success bool
		Error   *RPCError
	}
	if e := w.Call(ctx, "importdescriptors", &result, imports); e != nil {
		return nil, e
	}
	for _, r := range result {
		if !r.Success {
			return nil, fmt.Errorf("import failed: %v", r.Error)
		}
	}
	return w, nil
}
func (r *RPC) Unspent(ctx context.Context, addresses []string) ([]UTXO, error) {
	var list []UTXO
	e := r.Call(ctx, "listunspent", &list, 1, 9999999, addresses)
	return list, e
}
