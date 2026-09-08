package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/blakeswap/blakeswap/internal/authorization"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/storage"
	"github.com/blakeswap/blakeswap/internal/transport"
)

func TestNativeArchivedSignedSendRemainsAnExactRead(t *testing.T) {
	e, b, p := sendFixture(t)
	consentEngine(t, e)
	broadcasts := 0
	b.broadcast = func(string) (string, error) { broadcasts++; return "", context.DeadlineExceeded }
	raw, _ := json.Marshal(p)
	req := Request{Method: "wallet.send", Params: raw}
	want, err := e.Command(approveEngine(t, e, req), req)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.stageArchive("sends", p.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	e.Config.Authorization.Close()
	got, err := e.Command(context.Background(), req)
	if err != nil || !reflect.DeepEqual(got, want) || broadcasts != 1 || e.s.Sends[p.ID] != nil {
		t.Fatal("archived exact signed read acquired authority or stopped working", err)
	}
	detail, _ := json.Marshal(map[string]string{"kind": "send", "id": p.ID, "expected_wallet": "alice", "expected_network": "regtest"})
	result, err := e.Command(context.Background(), Request{Method: "record.get", Params: detail})
	if err != nil || !result.(RecordDetail).Archived || result.(RecordDetail).Send == nil {
		t.Fatal("cold detail requires new consent", err)
	}
	p.Amount--
	changed, _ := json.Marshal(p)
	if _, err = e.Command(context.Background(), Request{Method: "wallet.send", Params: changed}); !errors.Is(err, authorization.ErrRequired) {
		t.Fatal("archived ID authorized changed send", err)
	}
	readErr := errors.New("isolated cold read failed")
	e.archiveRead = func(string, string) (storage.ArchiveRecord, bool, error) {
		return storage.ArchiveRecord{}, false, readErr
	}
	if _, err = e.Command(context.Background(), req); !errors.Is(err, readErr) {
		t.Fatal("cold read error fell through consent", err)
	}
}

func TestNativeArchivedTradeReceiptRemainsAnExactRead(t *testing.T) {
	e, p := tradeFixture(t, "maker")
	q := requestQuote(t, e, p)
	confirm := confirmation(q)
	raw, _ := json.Marshal(confirm)
	req := Request{Method: "trade.confirm", Params: raw}
	consentEngine(t, e)
	want, err := e.Command(approveEngine(t, e, req), req)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.stageArchive("trade_receipts", confirm.RequestID); err != nil {
		t.Fatal(err)
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	e.Config.Authorization.Close()
	got, err := e.Command(context.Background(), req)
	if err != nil || !reflect.DeepEqual(got, want) || e.s.TradeReceipts[confirm.RequestID] != nil {
		t.Fatal("archived receipt needed new consent or became active", err)
	}
	confirm.Revision = confirm.Token
	changed, _ := json.Marshal(confirm)
	if _, err = e.Command(context.Background(), Request{Method: "trade.confirm", Params: changed}); !errors.Is(err, authorization.ErrRequired) {
		t.Fatal("archived receipt authorized changed terms", err)
	}
}

func TestNativeColdIncompleteAndUnknownIdentitiesNeedConsent(t *testing.T) {
	e, _, send := sendFixture(t)
	consentEngine(t, e)
	confirm := ConfirmTradeRequest{RequestID: transport.RandomID(), Token: transport.RandomID(), Revision: transport.RandomID(), ExpectedWallet: "alice", ExpectedNetwork: "regtest"}
	e.s.Sends = map[string]*WalletSend{send.ID: {PublicSend: PublicSend{ID: send.ID}, Digest: protocol.Digest(send)}}
	e.s.TradeReceipts = map[string]*TradeReceipt{confirm.RequestID: {Digest: protocol.Digest(confirm), Result: ConfirmTradeResult{ID: confirm.RequestID, State: "pending"}}}
	for _, key := range []storage.ArchiveKey{{Kind: "sends", ID: send.ID}, {Kind: "trade_receipts", ID: confirm.RequestID}} {
		if err := e.stageArchive(key.Kind, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.Config.Authorization.Close()
	for _, unknown := range []bool{false, true} {
		if unknown {
			send.ID = transport.RandomID()
			confirm.RequestID = transport.RandomID()
		}
		for method, request := range map[string]any{"wallet.send": send, "trade.confirm": confirm} {
			raw, _ := json.Marshal(request)
			if _, err := e.Command(context.Background(), Request{Method: method, Params: raw}); !errors.Is(err, authorization.ErrRequired) {
				t.Fatal("cold incomplete or unknown identity authorized work", method, unknown, err)
			}
		}
	}
}
