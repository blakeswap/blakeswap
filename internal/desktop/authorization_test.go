package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v2"
	"github.com/blakeswap/blakeswap/internal/api"
	"github.com/blakeswap/blakeswap/internal/daemon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nativeAPIFixture(t *testing.T) (*Manager, *api.Server) {
	t.Helper()
	m, _ := nativeManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root, err := os.MkdirTemp("", "bs-native-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	m.runtimeCtx, m.runtimeDir, m.servers = ctx, root, map[string]*api.Server{}
	if err := m.startAPI("alice"); err != nil {
		t.Fatal(err)
	}
	s := m.servers["alice"]
	t.Cleanup(s.Close)
	return m, s
}
func TestNativeDirectGRPCCLIAndHTTPRequireExactPrivateConsent(t *testing.T) {
	m, server := nativeAPIFixture(t)
	conn, err := grpc.NewClient("unix://"+server.Endpoint.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewDaemonServiceClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+server.Endpoint.Token)
	request := &pb.PrepareFirstWalletRequest{Name: "Isolated native wallet", Revision: m.settings.Revision}
	if _, err := client.PrepareFirstWallet(ctx, request); err == nil {
		t.Fatal("bearer alone installed wallet")
	}
	raw, _ := json.Marshal(request)
	if _, err := api.Call(context.Background(), server.Endpoint.Socket, daemon.Request{Method: "onboarding.prepare", Params: raw}); err == nil {
		t.Fatal("CLI bypassed native consent")
	}
	grant := nativeGrantID(t, m, "onboarding.prepare", request)
	request.Name = "Changed during authentication"
	if _, err := client.PrepareFirstWallet(metadata.AppendToOutgoingContext(ctx, "x-blakeswap-consent", grant), request); err == nil {
		t.Fatal("changed setup used original consent")
	}
	if _, err := os.Stat(filepath.Join(m.root, "wallets", "alice")); !os.IsNotExist(err) {
		t.Fatal("denied request installed profile")
	}
	grant = nativeGrantID(t, m, "onboarding.prepare", request)
	first, err := client.PrepareFirstWallet(metadata.AppendToOutgoingContext(ctx, "x-blakeswap-consent", grant), request)
	if err != nil {
		t.Fatal(err)
	}
	noPasswordFile(t, filepath.Join(m.root, "wallets", "alice"))
	post := func(grant string) (int, []byte) {
		t.Helper()
		r, err := http.NewRequest("POST", server.Endpoint.HTTP+"/v1/onboarding/recovery", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+server.Endpoint.Token)
		r.Header.Set("Content-Type", "application/json")
		if grant != "" {
			r.Header.Set("X-Blakeswap-Consent", grant)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	for _, grant := range []string{"", strings.Repeat("a", 64)} {
		code, body := post(grant)
		clear(body)
		if code < 400 {
			t.Fatal("direct HTTP disclosed recovery without private approval")
		}
	}
	recoveryGrant := nativeGrantID(t, m, "onboarding.get", &emptypb.Empty{})
	code, body := post(recoveryGrant)
	defer clear(body)
	if code != 200 {
		t.Fatal("exact private HTTP consent refused", code)
	}
	var recovered pb.FirstWallet
	if err := protojson.Unmarshal(body, &recovered); err != nil || recovered.Recovery.Mnemonic != first.Recovery.Mnemonic {
		t.Fatal("authorized recovery mismatch", err)
	}
	code, body = post(recoveryGrant)
	clear(body)
	if code < 400 {
		t.Fatal("HTTP replay reused consent")
	}
	if _, err := m.firstWallet(context.Background()); err == nil {
		t.Fatal("direct manager call bypassed consent")
	}
}
func TestNativeDirectExportAndSettingsContextCannotBypassConsent(t *testing.T) {
	m, _ := nativeManager(t)
	request := &pb.PrepareFirstWalletRequest{Name: "Native", Revision: m.settings.Revision}
	if _, err := m.prepareFirstWallet(nativeConsent(t, m, "onboarding.prepare", request), request); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "recovery.blakeswap")
	if _, err := m.exportPortable(context.Background(), "alice", destination, "isolated archive password", true); err == nil {
		t.Fatal("direct export bypassed consent")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("unauthorized export wrote recovery material")
	}
	export := &pb.ExportFirstWalletRequest{Path: destination, Password: "isolated archive password", Revision: m.settings.Revision}
	ctx := nativeConsent(t, m, "onboarding.export", export)
	m.settings.Revision++
	if _, err := m.exportFirstWallet(ctx, export); err == nil {
		t.Fatal("stale export context remained authorized")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("stale export wrote recovery material")
	}
}
