package chain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
)

const maxElectrumReply = 4 << 20

var errMissingCoinbase = errors.New("missing coinbase")

type Electrum struct {
	Network  Network
	ID       ID
	endpoint *url.URL
	pin      string
	mu       sync.Mutex
	conn     net.Conn
	reader   *bufio.Reader
	seq      uint64
	rangeMu  sync.Mutex
	ranges   map[uint32]headerRange
}

func NewElectrum(network Network, id ID, endpoint, pin string) (*Electrum, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if !network.Valid() || !id.Valid() || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" || u.Hostname() == "" {
		return nil, errors.New("Electrum endpoint requires ssl://host:port or tcp://loopback:port")
	}
	if _, err := net.LookupPort("tcp", u.Port()); err != nil {
		return nil, err
	}
	if u.Scheme != "ssl" && (u.Scheme != "tcp" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1")) {
		return nil, errors.New("public Electrum connections require TLS")
	}
	if pin != "" {
		if u.Scheme != "ssl" {
			return nil, errors.New("certificate pin requires a TLS endpoint")
		}
		b, e := hex.DecodeString(pin)
		if e != nil || len(b) != 32 {
			return nil, errors.New("certificate SHA256 must be 32 bytes in hex")
		}
		pin = strings.ToLower(pin)
	}
	return &Electrum{Network: network.Normalized(), ID: id, endpoint: u, pin: pin}, nil
}
func (e *Electrum) Close() error { e.mu.Lock(); defer e.mu.Unlock(); return e.disconnect() }
func (e *Electrum) disconnect() error {
	if e.conn != nil {
		err := e.conn.Close()
		e.conn = nil
		return err
	}
	return nil
}
func (e *Electrum) Call(ctx context.Context, method string, out any, params ...any) (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if e.conn == nil {
		d := net.Dialer{}
		if e.endpoint.Scheme == "ssl" {
			cfg := &tls.Config{ServerName: e.endpoint.Hostname(), MinVersion: tls.VersionTLS12}
			if e.pin != "" {
				// Explicit certificate pin replaces CA validation; never accept arbitrary certificates.
				cfg.InsecureSkipVerify = true
				cfg.VerifyConnection = func(s tls.ConnectionState) error {
					if len(s.PeerCertificates) == 0 {
						return errors.New("missing server certificate")
					}
					sum := sha256.Sum256(s.PeerCertificates[0].Raw)
					if hex.EncodeToString(sum[:]) != e.pin {
						return errors.New("Electrum certificate pin mismatch")
					}
					now := time.Now()
					cert := s.PeerCertificates[0]
					if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
						return errors.New("Electrum certificate expired or not yet valid")
					}
					return nil
				}
			}
			e.conn, err = (&tls.Dialer{NetDialer: &d, Config: cfg}).DialContext(ctx, "tcp", e.endpoint.Host)
		} else {
			e.conn, err = d.DialContext(ctx, "tcp", e.endpoint.Host)
		}
		if err != nil {
			return err
		}
		e.reader = bufio.NewReaderSize(e.conn, 8192)
	}
	defer func() {
		if err != nil {
			_ = e.disconnect()
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = e.conn.SetDeadline(deadline)
	// Cancellation interrupts an in-flight read, including shutdown of a desktop child.
	conn := e.conn
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
	}()
	e.seq++
	if params == nil {
		params = []any{}
	}
	if err = json.NewEncoder(e.conn).Encode(map[string]any{"jsonrpc": "2.0", "id": e.seq, "method": method, "params": params}); err != nil {
		return err
	}
	for notifications := 0; notifications < 64; notifications++ {
		var line []byte
		for {
			part, prefix, readErr := e.reader.ReadLine()
			if readErr != nil {
				return readErr
			}
			if len(line)+len(part) > maxElectrumReply {
				return errors.New("Electrum reply too large")
			}
			line = append(line, part...)
			if !prefix {
				break
			}
		}
		var reply struct {
			ID     *uint64         `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err = json.Unmarshal(line, &reply); err != nil {
			return err
		}
		if reply.ID == nil {
			continue
		}
		if *reply.ID != e.seq {
			return errors.New("Electrum reply ID mismatch")
		}
		if reply.Error != nil {
			return reply.Error
		}
		if len(reply.Result) == 0 {
			return errors.New("Electrum reply missing result")
		}
		if out != nil {
			return json.Unmarshal(reply.Result, out)
		}
		return nil
	}
	return errors.New("too many Electrum notifications")
}
func (e *Electrum) header(ctx context.Context, height uint32) ([]byte, error) {
	var raw string
	if err := e.Call(ctx, "blockchain.block.header", &raw, height); err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	return b, e.validateHeader(b, height)
}
func (e *Electrum) headerSize(height uint32) int {
	if e.ID == Blake && height >= e.Network.ForkHeight() {
		return 164
	}
	return 80
}
func (e *Electrum) validateHeader(b []byte, height uint32) error {
	expected := e.headerSize(height)
	if len(b) != expected {
		return fmt.Errorf("%s header at %d has %d bytes, expected %d; wrong chain or incompatible indexer", e.ID, height, len(b), expected)
	}
	v2 := binary.LittleEndian.Uint32(b[:4])&0x80000000 != 0
	if v2 != (expected == 164) {
		return errors.New("header version mismatch")
	}
	if expected == 164 && binary.LittleEndian.Uint32(b[128:132]) != height {
		return errors.New("Blake2b header height mismatch")
	}
	if err := verifyHeaderWork(b, e.Network); err != nil {
		return err
	}
	if height == 0 && chainhash.DoubleHashH(b).String() != e.Network.Genesis() {
		return errors.New("Electrum genesis header mismatch")
	}
	if e.ID == Blake && e.Network == Mainnet && height == e.Network.ForkHeight() {
		hash, err := HeaderHash(b)
		if err != nil {
			return err
		}
		if hash.String() != "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb" {
			return errors.New("Blake2b checkpoint mismatch")
		}
	}
	return nil
}
func (e *Electrum) Check(ctx context.Context) error {
	var features struct {
		Genesis string `json:"genesis_hash"`
		Hash    string `json:"hash_function"`
	}
	if err := e.Call(ctx, "server.features", &features); err != nil {
		return err
	}
	if features.Genesis != e.Network.Genesis() || features.Hash != "sha256" {
		return errors.New("Electrum network or script hash algorithm mismatch")
	}
	genesis, err := e.header(ctx, 0)
	if err != nil {
		return err
	}
	if chainhash.DoubleHashH(genesis).String() != e.Network.Genesis() {
		return errors.New("Electrum genesis header mismatch")
	}
	height, err := e.Height(ctx)
	if err != nil {
		return err
	}
	if height <= e.Network.ForkHeight() {
		return errors.New("chain has not passed Blake2b activation")
	}
	_, err = e.header(ctx, e.Network.ForkHeight())
	return err
}
func (e *Electrum) Height(ctx context.Context) (uint32, error) {
	height, _, err := e.tip(ctx)
	return height, err
}
func (e *Electrum) tip(ctx context.Context) (uint32, []byte, error) {
	var tip struct {
		Height uint32 `json:"height"`
		Hex    string `json:"hex"`
	}
	if err := e.Call(ctx, "blockchain.headers.subscribe", &tip); err != nil {
		return 0, nil, err
	}
	header, err := e.header(ctx, tip.Height)
	if err != nil {
		return 0, nil, err
	}
	if hex.EncodeToString(header) != tip.Hex {
		return 0, nil, errors.New("Electrum tip changed during read")
	}
	return tip.Height, header, nil
}
func (e *Electrum) Broadcast(ctx context.Context, raw string) (string, error) {
	tx, err := parseRaw(raw)
	if err != nil {
		return "", err
	}
	var id string
	err = e.Call(ctx, "blockchain.transaction.broadcast", &id, raw)
	if err == nil && id != tx.TxHash().String() {
		return "", errors.New("broadcast returned wrong transaction ID")
	}
	return id, err
}
func scriptHash(script []byte) string {
	h := sha256.Sum256(script)
	for i := 0; i < 16; i++ {
		h[i], h[31-i] = h[31-i], h[i]
	}
	return hex.EncodeToString(h[:])
}

type historyItem struct {
	TxID   string `json:"tx_hash"`
	Height int64  `json:"height"`
}

func (e *Electrum) history(ctx context.Context, script []byte) ([]historyItem, error) {
	var history []historyItem
	err := e.Call(ctx, "blockchain.scripthash.get_history", &history, scriptHash(script))
	if len(history) > 10000 {
		return nil, errors.New("script history exceeds bounded wallet capacity")
	}
	return history, err
}
func (e *Electrum) raw(ctx context.Context, id string) (Transaction, error) {
	var raw string
	if err := e.Call(ctx, "blockchain.transaction.get", &raw, id, false); err != nil {
		return Transaction{}, err
	}
	tx, err := parseRaw(raw)
	if err != nil {
		return Transaction{}, err
	}
	if tx.TxHash().String() != id {
		return Transaction{}, errors.New("indexer returned a different transaction")
	}
	return Transaction{Hex: raw, TxID: id}, nil
}
func (e *Electrum) inclusion(ctx context.Context, t Transaction, height uint32) (Transaction, error) {
	var proof struct {
		Height uint32   `json:"block_height"`
		Merkle []string `json:"merkle"`
		Pos    uint32   `json:"pos"`
	}
	if err := e.Call(ctx, "blockchain.transaction.get_merkle", &proof, t.TxID, height); err != nil {
		return t, err
	}
	if proof.Height != height || len(proof.Merkle) > 32 {
		return t, errors.New("invalid merkle proof height or depth")
	}
	h, err := chainhash.NewHashFromStr(t.TxID)
	if err != nil {
		return t, err
	}
	pos := proof.Pos
	for _, sibling := range proof.Merkle {
		other, err := chainhash.NewHashFromStr(sibling)
		if err != nil {
			return t, err
		}
		pair := make([]byte, 0, 64)
		if pos&1 == 0 {
			pair = append(pair, h[:]...)
			pair = append(pair, other[:]...)
		} else {
			pair = append(pair, other[:]...)
			pair = append(pair, h[:]...)
		}
		next := chainhash.DoubleHashH(pair)
		h = &next
		pos >>= 1
	}
	if pos != 0 {
		return t, errors.New("invalid merkle position")
	}
	header, err := e.header(ctx, height)
	if err != nil {
		return t, err
	}
	if !bytes.Equal(h[:], header[36:68]) {
		return t, errors.New("transaction merkle inclusion mismatch")
	}
	tip, tipHeader, err := e.tip(ctx)
	if err != nil {
		return t, err
	}
	if tip < height {
		return t, errors.New("transaction height above tip")
	}
	if err := e.connectHeaders(ctx, height, header, tip, tipHeader); err != nil {
		return t, err
	}
	// A repeated header read catches reorgs while the proof was being checked.
	current, err := e.header(ctx, height)
	if err != nil {
		return t, err
	}
	if !bytes.Equal(header, current) {
		return t, errors.New("chain changed during merkle verification")
	}
	currentTip, err := e.header(ctx, tip)
	if err != nil {
		return t, err
	}
	if !bytes.Equal(tipHeader, currentTip) {
		return t, errors.New("tip changed during merkle verification")
	}
	t.Height = height
	t.Confirmations = int(tip - height + 1)
	hash, _ := HeaderHash(header)
	t.BlockHash = hash.String()
	return t, nil
}
func (e *Electrum) Transaction(ctx context.Context, id string) (Transaction, error) {
	t, err := e.raw(ctx, id)
	if err != nil {
		return t, err
	}
	tx, _ := parseRaw(t.Hex)
	if len(tx.TxOut) == 0 {
		return t, errors.New("transaction has no outputs")
	}
	history, err := e.history(ctx, tx.TxOut[0].PkScript)
	if err != nil {
		return t, err
	}
	for _, item := range history {
		if item.TxID == id {
			if item.Height > 0 && item.Height <= int64(^uint32(0)) {
				return e.inclusion(ctx, t, uint32(item.Height))
			}
			if item.Height == 0 || item.Height == -1 {
				return t, nil
			}
			return t, errors.New("invalid transaction height")
		}
	}
	return t, errors.New("transaction missing from indexer history")
}
func (e *Electrum) Output(ctx context.Context, id string, vout uint32) (*TxOut, error) {
	t, err := e.raw(ctx, id)
	if err != nil {
		return nil, err
	}
	tx, _ := parseRaw(t.Hex)
	if uint64(vout) >= uint64(len(tx.TxOut)) {
		return nil, errors.New("invalid output index")
	}
	script := tx.TxOut[vout].PkScript
	coins, err := e.unspentScript(ctx, script)
	if err != nil {
		return nil, err
	}
	for _, coin := range coins {
		if coin.TxID == id && coin.Vout == vout {
			out := &TxOut{Value: coin.Amount, Confirmations: coin.Confirmations}
			out.Script.Hex = coin.Script
			return out, nil
		}
	}
	return nil, nil
}
func (e *Electrum) Observe(_ context.Context, _ string, _ []string) (Backend, error) { return e, nil }
func (e *Electrum) Unspent(ctx context.Context, addresses []string) ([]UTXO, error) {
	var result []UTXO
	for _, address := range addresses {
		a, err := btcutil.DecodeAddress(address, e.Network.Params())
		if err != nil || !a.IsForNet(e.Network.Params()) {
			return nil, errors.New("wrong wallet address network")
		}
		script, err := txscript.PayToAddrScript(a)
		if err != nil {
			return nil, err
		}
		coins, err := e.unspentScript(ctx, script)
		if err != nil {
			return nil, err
		}
		result = append(result, coins...)
	}
	return result, nil
}
func (e *Electrum) unspentScript(ctx context.Context, script []byte) ([]UTXO, error) {
	var coins []struct {
		TxID   string `json:"tx_hash"`
		Vout   uint32 `json:"tx_pos"`
		Height int64  `json:"height"`
		Value  int64  `json:"value"`
	}
	if err := e.Call(ctx, "blockchain.scripthash.listunspent", &coins, scriptHash(script)); err != nil {
		return nil, err
	}
	if len(coins) > 1000 {
		return nil, errors.New("UTXO count exceeds wallet capacity")
	}
	var result []UTXO
	seen := map[string]bool{}
	for _, coin := range coins {
		key := OutpointKey(coin.TxID, coin.Vout)
		if seen[key] {
			return nil, errors.New("duplicate indexer UTXO")
		}
		seen[key] = true
		t, err := e.raw(ctx, coin.TxID)
		if err != nil {
			return nil, err
		}
		tx, _ := parseRaw(t.Hex)
		if uint64(coin.Vout) >= uint64(len(tx.TxOut)) || coin.Value <= 0 || coin.Value > 2100000000000000 {
			return nil, errors.New("invalid indexer output")
		}
		out := tx.TxOut[coin.Vout]
		if out.Value != coin.Value || !bytes.Equal(out.PkScript, script) {
			return nil, errors.New("indexer UTXO differs from transaction")
		}
		if coin.Height > 0 && coin.Height <= int64(^uint32(0)) {
			t, err = e.inclusion(ctx, t, uint32(coin.Height))
			if err != nil {
				return nil, err
			}
		} else if coin.Height != 0 && coin.Height != -1 {
			return nil, errors.New("invalid UTXO height")
		}
		if isCoinbase(tx) && t.Confirmations < 100 {
			continue
		}
		result = append(result, UTXO{coin.TxID, coin.Vout, Coins(coin.Value), hex.EncodeToString(script), t.Confirmations})
	}
	return result, nil
}
func (e *Electrum) Coinbase(ctx context.Context, height uint32) (Transaction, error) {
	var id string
	if err := e.Call(ctx, "blockchain.transaction.id_from_pos", &id, height, 0, false); err != nil {
		return Transaction{}, err
	}
	t, err := e.raw(ctx, id)
	if err != nil {
		return t, err
	}
	return e.inclusion(ctx, t, height)
}
func (e *Electrum) Scan(ctx context.Context, start uint32, outpoints []string) (map[string]Observation, error) {
	result := map[string]Observation{}
	if len(outpoints) == 0 {
		return result, nil
	}
	tip, err := e.Height(ctx)
	if err != nil {
		return nil, err
	}
	before, err := e.header(ctx, tip)
	if err != nil {
		return nil, err
	}
	points := map[string]bool{}
	scripts := map[string][]byte{}
	for _, point := range outpoints {
		id, n, ok := strings.Cut(point, ":")
		if !ok {
			return nil, errors.New("invalid outpoint")
		}
		v, err := strconv.ParseUint(n, 10, 32)
		if err != nil {
			return nil, err
		}
		t, err := e.raw(ctx, id)
		if err != nil {
			// A tower must acknowledge a prepared rescue before funding is
			// published. Such an outpoint has no script history yet.
			if TransactionNotFound(err) {
				continue
			}
			return nil, err
		}
		tx, _ := parseRaw(t.Hex)
		if v >= uint64(len(tx.TxOut)) {
			return nil, errors.New("invalid outpoint index")
		}
		script := tx.TxOut[v].PkScript
		scripts[scriptHash(script)] = script
		points[point] = true
	}
	seen := map[string]bool{}
	for _, script := range scripts {
		history, err := e.history(ctx, script)
		if err != nil {
			return nil, err
		}
		for _, item := range history {
			if seen[item.TxID] || (item.Height > 0 && item.Height < int64(start)) {
				continue
			}
			seen[item.TxID] = true
			t, err := e.raw(ctx, item.TxID)
			if err != nil {
				return nil, err
			}
			tx, _ := parseRaw(t.Hex)
			for _, in := range tx.TxIn {
				key := OutpointKey(in.PreviousOutPoint.Hash.String(), in.PreviousOutPoint.Index)
				if !points[key] {
					continue
				}
				// The preimage is already known even if inclusion, another
				// history read, or the final reorg check later fails.
				if err := emitSpendWitness(ctx, key, tx); err != nil {
					return nil, err
				}
				if item.Height > 0 && item.Height <= int64(tip) {
					t, err = e.inclusion(ctx, t, uint32(item.Height))
					if err != nil {
						return nil, err
					}
				} else if item.Height != 0 && item.Height != -1 {
					return nil, errors.New("invalid spend height")
				}
				if _, exists := result[key]; exists {
					return nil, errors.New("conflicting indexer spends")
				}
				result[key] = Observation{t.TxID, tx, t.Height, t.Confirmations}
			}
		}
	}
	after, err := e.header(ctx, tip)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(before, after) {
		return nil, errors.New("chain changed during spend scan")
	}
	return result, nil
}

// ConfirmedReceived verifies a receipt even when all of its outputs were spent.
func (e *Electrum) ConfirmedReceived(ctx context.Context, address string) (bool, error) {
	a, err := btcutil.DecodeAddress(address, e.Network.Params())
	if err != nil || !a.IsForNet(e.Network.Params()) {
		return false, errors.New("wrong wallet address network")
	}
	script, err := txscript.PayToAddrScript(a)
	if err != nil {
		return false, err
	}
	history, err := e.history(ctx, script)
	if err != nil {
		return false, err
	}
	for _, item := range history {
		if item.Height <= 0 {
			continue
		}
		if item.Height > int64(^uint32(0)) {
			return false, errors.New("invalid receipt height")
		}
		t, err := e.raw(ctx, item.TxID)
		if err != nil {
			return false, err
		}
		t, err = e.inclusion(ctx, t, uint32(item.Height))
		if err != nil {
			return false, err
		}
		tx, err := parseRaw(t.Hex)
		if err != nil {
			return false, err
		}
		for _, out := range tx.TxOut {
			if t.Confirmations > 0 && out.Value > 0 && bytes.Equal(out.PkScript, script) {
				return true, nil
			}
		}
	}
	return false, nil
}
