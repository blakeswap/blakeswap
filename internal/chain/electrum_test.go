package chain

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
)

func TestLookupErrorsDistinguishEvictionFromTransportAndProofFailures(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{&RPCError{Code: -5, Message: "No such mempool or blockchain transaction"}, true},
		{&RPCError{Code: 1, Message: "transaction not found"}, true},
		{&RPCError{Code: 1, Message: "No such mempool or blockchain transaction. Use -txindex"}, true},
		{&RPCError{Code: 1, Message: "internal server error"}, false},
		{context.DeadlineExceeded, false},
	} {
		if got := TransactionNotFound(tt.err); got != tt.want {
			t.Fatalf("%v: %v", tt.err, got)
		}
	}
}
func TestElectrumRejectsTransportConfusion(t *testing.T) {
	for name, reply := range map[string]string{
		"wrong id":       `{"id":999,"result":1}`,
		"missing result": `{"id":1}`,
		"bad json":       `bad`,
	} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = bufio.NewReader(conn).ReadString('\n')
				_, _ = conn.Write([]byte(reply + "\n"))
			}()
			e, err := NewElectrum(Regtest, BTC, "tcp://"+listener.Addr().String(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			var out json.RawMessage
			if e.Call(context.Background(), "test", &out) == nil {
				t.Fatal("accepted invalid reply")
			}
			<-done
		})
	}
}
