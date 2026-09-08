package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestFundingAncestryTickOrderRetainsIndependentSlots(t *testing.T) {
	e := &Engine{s: State{Swaps: map[string]*Swap{}}}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		s := &Swap{ID: id, Role: "maker"}
		s.Short.Chain = chain.BTC
		if id == "c" || id == "e" {
			s.Role, s.Long.Chain = "taker", chain.Blake
		}
		if id != "b" && id != "f" {
			s.FundingParents = []FundingParent{{SwapID: "parent"}}
		}
		e.s.Swaps[id] = s
	}
	for _, want := range [][]string{{"a", "b", "c", "d", "e", "f"}, {"d", "b", "e", "a", "c", "f"}} {
		if got := e.swapTickIDs(); !reflect.DeepEqual(got, want) {
			t.Fatal("dependent rotation changed an unrelated slot or chain turn", got, want)
		}
	}
	delete(e.s.Swaps, "d")
	e.s.Swaps["e"].FundingParents = nil
	if got, want := e.swapTickIDs(), []string{"a", "b", "c", "e", "f"}; !reflect.DeepEqual(got, want) {
		t.Fatal("removed or independent prior turn pinned the next cycle", got, want)
	}
}

// Use the production RPC/Failover budget accounting with isolated synthetic
// chain metadata and real signed funding. This is not an actual-node test.
func TestFundingAncestryTickOrderSurvivesSharedBudgetExhaustion(t *testing.T) {
	e, children, nodes := ancestryGraph(t, "maker", chain.Blake)
	dependent := []*Swap{children[1], children[2]}
	previous := children[2]
	for i := 0; i < 7; i++ {
		own, raw, _ := localFunding(previous)
		tx, err := contract.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		input := chain.UTXO{TxID: own.TxID, Vout: 1, Amount: chain.Coins(tx.TxOut[1].Value), Script: hex.EncodeToString(tx.TxOut[1].PkScript), Confirmations: 200}
		next, _ := ancestryChild(t, e, "maker", chain.Blake, input)
		nextOwn, nextRaw, _ := localFunding(next)
		hash, _ := nodes[chain.Blake].BlockHash(context.Background(), 300)
		nodes[chain.Blake].transactions[nextOwn.TxID] = chain.Transaction{TxID: nextOwn.TxID, Hex: nextRaw, Height: 300, BlockHash: hash, Confirmations: 201}
		dependent = append(dependent, next)
		previous = next
	}
	sort.Slice(dependent, func(i, j int) bool { return dependent[i].ID < dependent[j].ID })
	healthy := dependent[len(dependent)-1]
	missing := map[string]bool{}
	for _, s := range dependent[:len(dependent)-1] {
		own, _, _ := localFunding(s)
		missing[own.TxID] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "getblockchaininfo":
			result = map[string]any{"chain": "regtest", "blocks": 500, "bestblockhash": fmt.Sprintf("%064x", 500)}
		case "getblockcount":
			result = 500
		case "getblockhash":
			var height uint32
			_ = json.Unmarshal(request.Params[0], &height)
			result = fmt.Sprintf("%064x", height)
			if height == 0 {
				result = chain.Regtest.Genesis()
			}
		case "getblockheader":
			result = map[string]any{"height": 300}
			if len(request.Params) > 1 && string(request.Params[1]) == "false" {
				result = strings.Repeat("00", 164)
			}
		case "getdeploymentinfo":
			result = map[string]any{"blake2b": map[string]any{"active": true, "height": 1}}
		case "getrawtransaction":
			var id string
			_ = json.Unmarshal(request.Params[0], &id)
			if missing[id] {
				select {
				case <-time.After(30 * time.Millisecond):
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": -5, "message": "not found"}})
				case <-r.Context().Done():
				}
				return
			}
			result = nodes[chain.Blake].transactions[id]
		default:
			t.Errorf("unexpected RPC %s", request.Method)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	}))
	t.Cleanup(server.Close)
	cookie := filepath.Join(t.TempDir(), "cookie")
	if err := os.WriteFile(cookie, []byte("test:test"), 0600); err != nil {
		t.Fatal(err)
	}
	pool, err := chain.NewFailover(chain.Regtest, chain.Blake, chain.Endpoint{Kind: "rpc", URL: server.URL, Cookie: cookie})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	e.nodes[chain.Blake] = pool
	refresh := func() {
		t.Helper()
		if _, err := pool.Height(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.chainGeneration[chain.Blake] = pool.Generation()
		e.chainFresh[chain.Blake] = true
		e.fundingAncestryProofs = nil
	}
	refresh()
	if err := e.refreshFundingAncestry(context.Background(), healthy); err != nil {
		t.Fatal("healthy exact funding failed standalone control", err)
	}
	reached := false
	for pass := 0; pass < len(dependent); pass++ {
		refresh()
		// Scale the production 8s allowance to 160ms: eight 30ms missing
		// reads exceed it. Keep the real per-child timeout and budget code.
		ctx := chain.WithWorkBudgets(context.Background(), 160*time.Millisecond)
		for _, id := range e.swapTickIDs() {
			s := e.s.Swaps[id]
			err := e.refreshFundingAncestry(ctx, s)
			if s == healthy && err == nil {
				reached = true
			}
			own, _, _ := localFunding(s)
			if missing[own.TxID] && (err == nil || e.fundingAncestryReady(s)) {
				t.Fatal("slow missing funding became positive evidence")
			}
		}
	}
	if !reached {
		t.Fatal("healthy exact funding starved despite a turn for every dependent child")
	}
}

func TestFundingAncestryOrderPreservesObservedSpendProgress(t *testing.T) {
	e, children, _ := fundedFillPair(t, chain.Blake)
	original := children[0]
	all := map[chain.ID]map[string]chain.Observation{chain.Blake: {}}
	b := &reviewEvidenceBudgetBackend{Backend: &fundingLookupBackend{err: context.DeadlineExceeded}, records: map[string]chain.Transaction{}}
	e.nodes[chain.Blake] = b
	// This tests only bounded proof scheduling. Copies keep the real signing
	// authority and unique outpoints; no incomplete copied graph is persisted.
	e.s.Swaps = map[string]*Swap{}
	var candidates []*Swap
	for i := 0; i < 3; i++ {
		s := *original
		s.ID, s.Short.TxID = transport.RandomID(), transport.RandomID()
		s.FundingParents = []FundingParent{{SwapID: "scheduling-only"}}
		e.s.Swaps[s.ID] = &s
		candidates = append(candidates, &s)
		count := 128
		if i == 2 {
			count = 3
		}
		obs, previous := reviewManyInputRefund(t, e, original, s.Short, count)
		for j, p := range previous {
			if i < 2 && j == len(previous)-1 {
				continue
			}
			b.records[p.TxID] = p
		}
		all[chain.Blake][chain.OutpointKey(s.Short.TxID, s.Short.Vout)] = obs
	}
	for scan := 0; scan < 4; scan++ {
		e.resetObservedSpendWork()
		for _, id := range e.swapTickIDs() {
			e.prepareObservedSpends(context.Background(), e.s.Swaps[id], all)
		}
		if e.observedSpendReads > observedPrevoutReads {
			t.Fatal("proof scheduler exceeded the per-pass read budget", e.observedSpendReads)
		}
		for _, s := range candidates[:2] {
			obs, _ := observation(all, s.Short)
			if e.validateContractObservation(s.Short, obs) == nil {
				t.Fatal("unavailable ancestor input became qualified evidence")
			}
		}
	}
	healthy := candidates[2]
	obs, _ := observation(all, healthy.Short)
	if err := e.validateContractObservation(healthy.Short, obs); err != nil {
		t.Fatal("two failed prefixes starved the third candidate across four scans", err)
	}
}
