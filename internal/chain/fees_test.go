package chain

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFeeDecimalsAreExactAndBounded(t *testing.T) {
	for text, want := range map[string]int64{"0.00006539": 6539, "0.00001": 1000, "0.000001": 100, "0.00000000001": 1, "1e-8": 1, "0.12345678": 12345678} {
		got, err := feeRate(json.Number(text))
		if err != nil || got != want {
			t.Fatalf("%s = %d (%v), want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"-1", "0", "NaN", "1e100000000", "1e99", "1e-99", "99999999999999999999", "1/2", "0.0000000000001", "1e999999999999999999999"} {
		if _, err := feeRate(json.Number(text)); err == nil {
			t.Fatal("accepted", text)
		}
	}
}

func TestRPCFeeUnitsSourceEffectiveTargetAndFailure(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		reply := `{"feerate":0.00006539,"blocks":8}`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"result":%s,"error":null,"id":1}`, reply)
		}))
		cookie := filepath.Join(t.TempDir(), "cookie")
		if err := os.WriteFile(cookie, []byte("user:password"), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := New(id, server.URL, cookie)
		if err != nil {
			t.Fatal(err)
		}
		f := r.EstimateFee(context.Background(), 6)
		if f.Chain != id || f.Rate != 6539 || f.Target != 8 || f.RequestedTarget != 6 || !f.Current(time.Now()) {
			t.Fatalf("bad native estimate: %+v", f)
		}
		reply = `{"errors":["Insufficient data"],"blocks":6}`
		f = r.EstimateFee(context.Background(), 6)
		if f.State != "unavailable" || f.Rate != 0 || f.Error == "" {
			t.Fatal(f)
		}
		server.Close()
		r.Close()
	}
}

func TestElectrumFeeUnitsAndUnavailable(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		for _, value := range []string{"0.00001", "-1"} {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var p struct {
					Method string
					ID     uint64
				}
				line, _ := bufio.NewReader(conn).ReadBytes('\n')
				_ = json.Unmarshal(line, &p)
				if p.Method != "blockchain.estimatefee" {
					return
				}
				fmt.Fprintf(conn, "{\"id\":%d,\"result\":%s}\n", p.ID, value)
			}()
			e, err := NewElectrum(Regtest, id, "tcp://"+listener.Addr().String(), "")
			if err != nil {
				t.Fatal(err)
			}
			f := e.EstimateFee(context.Background(), 6)
			if f.Chain != id || f.Source != "electrum:blockchain.estimatefee" {
				t.Fatal(f)
			}
			if value == "-1" {
				if f.State != "unavailable" || f.Rate != 0 {
					t.Fatal(f)
				}
			} else if f.Rate != 1000 || !f.Current(time.Now()) {
				t.Fatal(f)
			}
			e.Close()
			listener.Close()
			<-done
		}
	}
}

func TestFeeFreshness(t *testing.T) {
	now := time.Now()
	f := FeeEstimate{Rate: 1000, Timestamp: now.Unix(), State: "available"}
	if !f.Current(now) {
		t.Fatal("fresh unavailable")
	}
	for _, stamp := range []int64{now.Unix() + 1, now.Unix() - 121} {
		f.Timestamp = stamp
		if f.Current(now) {
			t.Fatal("stale/future accepted")
		}
	}
}
