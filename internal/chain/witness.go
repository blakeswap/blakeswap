package chain

import (
	"context"

	"github.com/btcsuite/btcd/wire"
)

// SpendWitness contains transaction bytes learned from a validated source with
// a matching transaction ID, associated with a watched outpoint. It conveys no canonicality,
// confirmation count, or evidence that any other output is unspent. Consumers
// must validate the contract/preimage before treating it as immutable knowledge.
type SpendWitness struct {
	Outpoint string
	Tx       *wire.MsgTx
}

type witnessSinkKey struct{}
type witnessSink func(SpendWitness) error
type witnessSinkError struct{ cause error }

func (e *witnessSinkError) Error() string { return e.cause.Error() }
func (e *witnessSinkError) Unwrap() error { return e.cause }

// WithSpendWitnessSink receives each decoded spend before further network IO,
// even when the enclosing snapshot subsequently fails or changes sources. The
// sink runs synchronously and must durably retain its facts before returning.
// A sink failure stops scanning and failover; it is not an endpoint failure.
func WithSpendWitnessSink(ctx context.Context, sink func(SpendWitness) error) context.Context {
	return context.WithValue(ctx, witnessSinkKey{}, witnessSink(sink))
}
func emitSpendWitness(ctx context.Context, key string, tx *wire.MsgTx) error {
	sink, _ := ctx.Value(witnessSinkKey{}).(witnessSink)
	if sink == nil {
		return nil
	}
	if err := sink(SpendWitness{Outpoint: key, Tx: tx}); err != nil {
		return &witnessSinkError{cause: err}
	}
	return nil
}
