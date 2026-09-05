package chain

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opt-in, read-only checks. Never broadcast or publish on a public service in tests.
func TestPublishedElectrumServices(t *testing.T) {
	if os.Getenv("BLAKESWAP_LIVE_READS") != "1" {
		t.Skip("set BLAKESWAP_LIVE_READS=1 for public endpoint checks")
	}
	for _, c := range []struct {
		n        Network
		id       ID
		url, pin string
	}{
		{Mainnet, BTC, "ssl://electrum.blockstream.info:50002", ""},
		{Mainnet, Blake, "ssl://fulcrum.kilombino.com:17717", "506dadc710c5abaeb13191056c5aaf47035d30e08bd869f7b4fbe6e13745d5a7"},
		{Testnet, BTC, "ssl://mempool.space:40002", ""},
	} {
		t.Run(string(c.n)+"/"+string(c.id), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			b, err := NewElectrum(c.n, c.id, c.url, c.pin)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if err = b.Check(ctx); err != nil {
				t.Fatal(err)
			}
			height, err := b.Height(ctx)
			if err != nil {
				t.Fatal(err)
			}
			coin, err := b.Coinbase(ctx, height-6)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := parseRaw(coin.Hex)
			if err != nil || !isCoinbase(tx) || !coinbaseHeight(tx, coin.Height) {
				t.Fatal("invalid coinbase", err)
			}
			t.Logf("height=%d coinbase merkle verified", height)
		})
	}
}
