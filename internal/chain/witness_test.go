package chain

import (
	"context"
	"errors"
	"github.com/btcsuite/btcd/wire"
	"testing"
)

type witnessScannerFunc func(context.Context, uint32, []string) (map[string]Observation, error)

func (s witnessScannerFunc) Scan(c context.Context, h uint32, p []string) (map[string]Observation, error) {
	return s(c, h, p)
}

func TestFailoverWitnessSurvivesFailedAttemptAndStopsOnSinkFailure(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, failSink := range []bool{false, true} {
			t.Run(map[bool]string{false: "tower", true: "local"}[local]+map[bool]string{false: "/fallback", true: "/sink-failure"}[failSink], func(t *testing.T) {
				p := testPool(&failoverBackend{height: 100}, &failoverBackend{height: 100})
				delivered, fallback := 0, 0
				first := witnessScannerFunc(func(ctx context.Context, _ uint32, _ []string) (map[string]Observation, error) {
					if err := emitSpendWitness(ctx, "watched:0", wire.NewMsgTx(2)); err != nil {
						return nil, err
					}
					return nil, errors.New("transport failed after witness")
				})
				second := witnessScannerFunc(func(context.Context, uint32, []string) (map[string]Observation, error) {
					fallback++
					if delivered != 1 {
						t.Fatal("fallback started before witness was saved")
					}
					return map[string]Observation{}, nil
				})
				var scanner SpendScanner = &failoverScanner{pool: p, scanners: []SpendScanner{first, second}}
				if local {
					p.entries[0].scanner = first
					p.entries[1].scanner = second
					scanner = p
				}
				ctx := WithSpendWitnessSink(context.Background(), func(SpendWitness) error {
					delivered++
					if failSink {
						return context.DeadlineExceeded
					}
					return nil
				})
				result, err := scanner.Scan(ctx, 1, []string{"watched:0"})
				if delivered != 1 {
					t.Fatal("witness lost", delivered)
				}
				if failSink {
					var sink *witnessSinkError
					if !errors.As(err, &sink) || fallback != 0 || result != nil || p.Status().Endpoints[0].Error != "" {
						t.Fatal("durability error retried or poisoned transport", err, fallback, p.Status())
					}
				} else if err != nil || result == nil || len(result) != 0 || fallback != 1 || p.Status().Endpoints[0].Error == "" {
					t.Fatal("failed first attempt hid witness or partial canonical map escaped", err, fallback, p.Status())
				}
			})
		}
	}
}
