package chain

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func mineHeader(t *testing.T, id ID, height uint32, parent []byte, bits uint32) []byte {
	t.Helper()
	size, version := 80, uint32(1)
	if id == Blake && height > 0 {
		size, version = 164, 0x80000000
	}
	h := make([]byte, size)
	binary.LittleEndian.PutUint32(h, version)
	if parent != nil {
		hash, err := HeaderHash(parent)
		if err != nil {
			t.Fatal(err)
		}
		copy(h[4:36], hash[:])
	}
	binary.LittleEndian.PutUint32(h[68:72], 1700000000+height)
	binary.LittleEndian.PutUint32(h[72:76], bits)
	if size == 164 {
		binary.LittleEndian.PutUint32(h[128:132], height)
	}
	remineHeader(t, h)
	return h
}
func remineHeader(t *testing.T, h []byte) {
	t.Helper()
	target := blockchain.CompactToBig(binary.LittleEndian.Uint32(h[72:76]))
	for nonce := uint32(0); ; nonce++ {
		binary.LittleEndian.PutUint32(h[76:80], nonce)
		hash, err := HeaderHash(h)
		if err != nil {
			t.Fatal(err)
		}
		value, _ := new(big.Int).SetString(hash.String(), 16)
		if value.Cmp(target) <= 0 {
			return
		}
		if nonce == 10000 {
			t.Fatal("test header unexpectedly difficult")
		}
	}
}

func electrumFixture(t *testing.T, network Network, id ID, reply func(string, []json.RawMessage) any) *Electrum {
	t.Helper()
	client, server := net.Pipe()
	e := &Electrum{Network: network, ID: id, conn: client, reader: bufio.NewReader(client)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		decoder := json.NewDecoder(server)
		for {
			var req struct {
				ID     uint64
				Method string
				Params []json.RawMessage
			}
			if decoder.Decode(&req) != nil {
				return
			}
			if json.NewEncoder(server).Encode(map[string]any{"id": req.ID, "result": reply(req.Method, req.Params)}) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { e.Close(); <-done })
	return e
}

func TestElectrumEnforcesNetworkWorkLimit(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		for _, network := range []Network{Mainnet, Testnet, Regtest} {
			t.Run(string(id)+"/"+string(network), func(t *testing.T) {
				height := network.ForkHeight() + 1
				for _, bits := range []uint32{0x207fffff, 0x2100ffff} {
					header := mineHeader(t, id, height, nil, bits)
					e := electrumFixture(t, network, id, func(string, []json.RawMessage) any { return hex.EncodeToString(header) })
					_, err := e.header(context.Background(), height)
					if network == Regtest && bits == 0x207fffff {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "network limit") {
						t.Fatalf("accepted target %x or wrong error: %v", bits, err)
					}
				}
			})
		}
	}
	for _, bits := range []uint32{0, 0x1d80ffff, 0x23000001, 0x01000001} {
		h := make([]byte, 80)
		binary.LittleEndian.PutUint32(h[72:76], bits)
		if verifyHeaderWork(h, Regtest) == nil {
			t.Fatalf("accepted invalid target %x", bits)
		}
	}
}

func TestElectrumVerifiesConnectedConfirmationsAndMedianTime(t *testing.T) {
	for _, id := range []ID{BTC, Blake} {
		for _, fault := range []string{"none", "disconnected", "wrong tip", "short batch", "empty batch", "oversized batch", "truncated batch", "trailing batch", "median reorg", "confirmation reorg"} {
			t.Run(string(id)+"/"+fault, func(t *testing.T) {
				headers := make(map[uint32][]byte)
				txid := chainhash.DoubleHashH([]byte("transaction"))
				headers[1] = mineHeader(t, id, 1, nil, 0x207fffff)
				copy(headers[1][36:68], txid[:])
				remineHeader(t, headers[1])
				for h := uint32(2); h <= 15; h++ {
					headers[h] = mineHeader(t, id, h, headers[h-1], 0x207fffff)
				}
				if fault == "disconnected" {
					headers[7] = mineHeader(t, id, 7, nil, 0x207fffff)
				}
				tipReads := 0
				e := electrumFixture(t, Regtest, id, func(method string, p []json.RawMessage) any {
					num := func(i int) uint32 { var n uint32; json.Unmarshal(p[i], &n); return n }
					switch method {
					case "blockchain.block.header":
						h := num(0)
						if h == 15 {
							tipReads++
							if (fault == "median reorg" && tipReads >= 3) || (fault == "confirmation reorg" && tipReads >= 2) {
								return hex.EncodeToString(mineHeader(t, id, 15, nil, 0x207fffff))
							}
						}
						return hex.EncodeToString(headers[h])
					case "blockchain.headers.subscribe":
						return map[string]any{"height": 15, "hex": hex.EncodeToString(headers[15])}
					case "blockchain.transaction.get_merkle":
						return map[string]any{"block_height": 1, "merkle": []string{}, "pos": 0}
					case "blockchain.block.headers":
						start, count := num(0), num(1)
						if fault == "short batch" {
							count = min(count, 3)
						}
						var raw []byte
						for h := start; h < start+count; h++ {
							next := headers[h]
							if fault == "wrong tip" && h == 15 {
								next = mineHeader(t, id, 15, headers[14], 0x207fffff)
								next[36] ^= 1
								remineHeader(t, next)
							}
							raw = append(raw, next...)
						}
						switch fault {
						case "empty batch":
							count = 0
							raw = nil
						case "oversized batch":
							count++
						case "truncated batch":
							raw = raw[:len(raw)-1]
						case "trailing batch":
							raw = append(raw, 0)
						}
						return map[string]any{"count": count, "hex": hex.EncodeToString(raw)}
					}
					return nil
				})
				if fault == "none" || fault == "disconnected" || fault == "median reorg" {
					median, err := e.MedianTime(context.Background())
					if fault == "none" {
						if err != nil || median != 1700000010 {
							t.Fatalf("median %d: %v", median, err)
						}
					} else if err == nil {
						t.Fatal("invalid median time accepted")
					}
				}
				if fault == "median reorg" {
					return
				}
				tx, err := e.inclusion(context.Background(), Transaction{TxID: txid.String()}, 1)
				if fault == "none" || fault == "short batch" {
					if err != nil || tx.Confirmations != 15 {
						t.Fatalf("confirmations %d: %v", tx.Confirmations, err)
					}
				} else if err == nil {
					t.Fatal("forged confirmations accepted")
				}
			})
		}
	}
}

func TestElectrumHeaderCacheChecksReorgAndExtension(t *testing.T) {
	headers := map[uint32][]byte{}
	for h := uint32(1); h <= 5; h++ {
		headers[h] = mineHeader(t, BTC, h, headers[h-1], 0x207fffff)
	}
	batches := 0
	e := electrumFixture(t, Regtest, BTC, func(method string, p []json.RawMessage) any {
		var start, count uint32
		json.Unmarshal(p[0], &start)
		if method == "blockchain.block.header" {
			return hex.EncodeToString(headers[start])
		}
		json.Unmarshal(p[1], &count)
		batches++
		var raw []byte
		for h := start; h < start+count; h++ {
			raw = append(raw, headers[h]...)
		}
		return map[string]any{"count": count, "hex": hex.EncodeToString(raw)}
	})
	for _, end := range []uint32{4, 4, 5} {
		if err := e.connectHeaders(context.Background(), 1, headers[1], end, headers[end]); err != nil {
			t.Fatal(err)
		}
	}
	if batches != 2 {
		t.Fatal("completed range was not reused", batches)
	}
	headers[3] = mineHeader(t, BTC, 3, nil, 0x207fffff)
	headers[4] = mineHeader(t, BTC, 4, headers[3], 0x207fffff)
	headers[5] = mineHeader(t, BTC, 5, headers[4], 0x207fffff)
	if e.connectHeaders(context.Background(), 1, headers[1], 5, headers[5]) == nil {
		t.Fatal("cached range survived disconnected reorg")
	}
}

func TestElectrumResumesInterruptedHeaderRanges(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		t.Run(fmt.Sprint("reorg=", reorg), func(t *testing.T) {
			headers := map[uint32][]byte{}
			for h := uint32(1); h <= 5; h++ {
				headers[h] = mineHeader(t, BTC, h, headers[h-1], 0x207fffff)
			}
			var starts []uint32
			interrupted := false
			e := electrumFixture(t, Regtest, BTC, func(method string, p []json.RawMessage) any {
				var start, count uint32
				json.Unmarshal(p[0], &start)
				if method == "blockchain.block.header" {
					return hex.EncodeToString(headers[start])
				}
				json.Unmarshal(p[1], &count)
				starts = append(starts, start)
				if len(starts) == 2 {
					interrupted = true
					return map[string]any{"count": 0, "hex": ""}
				}
				count = min(count, 2)
				var raw []byte
				for h := start; h < start+count; h++ {
					raw = append(raw, headers[h]...)
				}
				return map[string]any{"count": count, "hex": hex.EncodeToString(raw)}
			})
			if err := e.connectHeaders(context.Background(), 1, headers[1], 5, headers[5]); err == nil || !interrupted {
				t.Fatal("partial proof returned success", err)
			}
			if cached, ok := e.ranges[1]; !ok || cached.end != 3 {
				t.Fatal("verified progress lost on interruption")
			}
			if reorg {
				headers[2][36] ^= 1
				remineHeader(t, headers[2])
				for h := uint32(3); h <= 5; h++ {
					headers[h] = mineHeader(t, BTC, h, headers[h-1], 0x207fffff)
				}
			}
			if err := e.connectHeaders(context.Background(), 1, headers[1], 5, headers[5]); err != nil {
				t.Fatal(err)
			}
			want := uint32(4)
			if reorg {
				want = 2
			}
			if starts[2] != want {
				t.Fatalf("resumed from %d, want %d after reorg=%v", starts[2], want, reorg)
			}
		})
	}
}
