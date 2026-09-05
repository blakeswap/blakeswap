package chain

import (
	"encoding/json"
	"testing"
)

func TestExactAmounts(t *testing.T) {
	for _, s := range []string{"0.00000001", "21000000.00000000", "1.23456789", "-0.00000001", "0.00000000"} {
		var n Coins
		if e := json.Unmarshal([]byte(s), &n); e != nil {
			t.Fatal(e)
		}
		out, e := json.Marshal(n)
		if e != nil || string(out) != s {
			t.Fatalf("%s -> %s", s, out)
		}
	}
	for _, s := range []string{"1e8", "0.000000001", "21000000.00000001", "99999999999999999999999999"} {
		var n Coins
		if json.Unmarshal([]byte(s), &n) == nil {
			t.Fatal("accepted", s)
		}
	}
}
func TestRPCRejectsExternal(t *testing.T) {
	for _, u := range []string{"http://example.com:8332", "https://127.0.0.1:19443", "http://localhost:19443", "http://name:pass@127.0.0.1"} {
		if _, e := New(BTC, u, "unused"); e == nil {
			t.Fatal("accepted", u)
		}
	}
}
