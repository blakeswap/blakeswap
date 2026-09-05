package chain

import (
	"context"
	"errors"
	"strings"
)

// TransactionNotFound only classifies explicit lookup failures. Transport and
// proof errors must stop recovery rather than be mistaken for mempool eviction.
func TransactionNotFound(err error) bool {
	var rpc *RPCError
	if !errors.As(err, &rpc) {
		return false
	}
	if rpc.Code == -5 {
		return true
	}
	if rpc.Code != 1 {
		return false
	}
	message := strings.ToLower(rpc.Message)
	return strings.Contains(message, "no such mempool or blockchain transaction") ||
		message == "no such transaction" || message == "transaction not found"
}

// Backend is also the trust boundary for chain observations. Electrum verifies
// transactions and merkle inclusion but relies on its operator for canonicality,
// completeness, and chain work. A user-controlled full node validates consensus.
type Backend interface {
	Check(context.Context) error
	Height(context.Context) (uint32, error)
	MedianTime(context.Context) (uint32, error)
	Broadcast(context.Context, string) (string, error)
	Output(context.Context, string, uint32) (*TxOut, error)
	Transaction(context.Context, string) (Transaction, error)
	Observe(context.Context, string, []string) (Backend, error)
	Unspent(context.Context, []string) ([]UTXO, error)
	Coinbase(context.Context, uint32) (Transaction, error)
	Close() error
}
type SpendScanner interface {
	Scan(context.Context, uint32, []string) (map[string]Observation, error)
}

func (r *RPC) Close() error { r.client.CloseIdleConnections(); return nil }
func (r *RPC) Coinbase(ctx context.Context, height uint32) (Transaction, error) {
	var hash string
	if err := r.Call(ctx, "getblockhash", &hash, height); err != nil {
		return Transaction{}, err
	}
	var block struct{ Tx []string }
	if err := r.Call(ctx, "getblock", &block, hash, 1); err != nil {
		return Transaction{}, err
	}
	if len(block.Tx) == 0 {
		return Transaction{}, errMissingCoinbase
	}
	var tx Transaction
	err := r.Call(ctx, "getrawtransaction", &tx, block.Tx[0], true, hash)
	tx.Height = height
	return tx, err
}
