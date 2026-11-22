/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/mnemonic"
	"github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/TxnLab/zerosignal/go/solana/zsescrow"
	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

type stubSigner struct{}

func (stubSigner) Address() string                                           { return "PAYER" }
func (stubSigner) HasAccount(a string) bool                                  { return a == "PAYER" }
func (stubSigner) SignBytes(context.Context, string, []byte) ([]byte, error) { return nil, nil }
func (stubSigner) SignWithAccount(context.Context, types.Transaction, string) (string, []byte, error) {
	return "", nil, nil
}

type stubReserve struct{}

func (stubReserve) Reserve(context.Context, zs.Operator, ticket.ReserveRequest, *zs.RelayHop) (*zs.ReservedTicket, error) {
	return nil, errors.New("stub")
}

type stubWallet struct{}

func (stubWallet) Open(context.Context, *ticket.Ticket, zs.Operator, string) (*zs.OpenHandle, error) {
	return nil, errors.New("stub")
}
func (stubWallet) PayerAddr() string { return "PAYER" }

// stubClient is a Client whose network parts are all supplied, so New reads no
// chain.
func stubClient(t *testing.T) *zs.Client {
	t.Helper()
	reg, err := zs.NewStaticRegistryProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := zs.New(context.Background(), zs.Config{
		Signer:        stubSigner{},
		Network:       zs.Network{Algod: algod.Config{Endpoint: "http://algod.invalid"}, EscrowAppID: 1},
		Registry:      reg,
		ReserveClient: stubReserve{},
		Wallet:        stubWallet{},
		Logger:        quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// The served server carries the handler and, with the daemon's token, the
// Windows graceful-stop route.
func TestServeClient_ServesTheHandlerAndTheShutdownRoute(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:9999"
	fired := make(chan struct{}, 1)
	h, srv := serveClient(stubClient(t), cfg, Options{ShutdownToken: "tok"}, func() { fired <- struct{}{} }, quietLogger())
	if h == nil || srv.Addr != "127.0.0.1:9999" {
		t.Fatalf("handler %v, addr %q", h, srv.Addr)
	}

	health := httptest.NewRecorder()
	srv.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Errorf("GET /healthz: status %d, want the handler's 200", health.Code)
	}

	if rec := postShutdown(srv.Handler, "127.0.0.1:5", "tok"); rec.Code != http.StatusOK {
		t.Fatalf("POST /shutdown with the token: status %d, want 200", rec.Code)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("POST /shutdown did not trigger the shutdown")
	}
}

// The handler is told the web app's URL, and -times reaches it.
func TestHandlerArgs(t *testing.T) {
	for _, track := range []bool{false, true} {
		hc, ho := handlerArgs(Options{TrackCryptoTimes: track})
		if hc.FundingURL != build.ClientURL || build.ClientURL == "" {
			t.Errorf("FundingURL %q, want %q", hc.FundingURL, build.ClientURL)
		}
		if ho.TrackCryptoTimes != track {
			t.Errorf("TrackCryptoTimes %v, want %v", ho.TrackCryptoTimes, track)
		}
	}
}

type mnemonicProvider struct{ m string }

func (p mnemonicProvider) Mnemonics(context.Context) ([]keystore.MnemonicEntry, error) {
	return []keystore.MnemonicEntry{{Name: "payer", Mnemonic: p.m}}, nil
}

// runConfig is a config and options Run accepts with build as its client
// builder: one keystore account, logs discarded, no catalog file.
func runConfig(t *testing.T, build func(context.Context, zs.Config) (*zs.Client, error)) (*config.Config, Options) {
	t.Helper()
	m, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := keystore.New(context.Background(), mnemonicProvider{m})
	if err != nil {
		t.Fatal(err)
	}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfg := &config.Config{}
	cfg.Zs.CatalogPath = "off"
	cfg.Algod.Network = "testnet"
	cfg.Zs.EscrowAppID = 4242
	dir := t.TempDir()
	return cfg, Options{Keystore: ks, LogWriter: io.Discard, buildClient: build,
		stateDir: func() (string, error) { return dir, nil }}
}

// runJournalPath is the journal Run opens for runConfig's cfg and opts.
func runJournalPath(t *testing.T, opts Options) string {
	t.Helper()
	dir, _ := opts.stateDir()
	return filepath.Join(dir, "ticket-journal-algorand-testnet-4242.json")
}

// Run hands the SDK the journal for its escrow app, and releases the journal
// when it returns, whether or not the client started.
func TestRun_JournalsInTheStateDirAndReleasesIt(t *testing.T) {
	var got zs.Config
	var heldErr error
	var opts Options
	cfg, opts := runConfig(t, func(_ context.Context, zc zs.Config) (*zs.Client, error) {
		got = zc
		_, heldErr = zs.OpenFileJournal(runJournalPath(t, opts))
		return nil, errors.New("algod down")
	})
	_ = Run(context.Background(), cfg, opts)
	if !errors.Is(heldErr, zs.ErrJournalInUse) {
		t.Errorf("opening this app's journal during startup: %v, want ErrJournalInUse", heldErr)
	}
	if got.Stores.Journal == nil {
		t.Fatal("Run gave the SDK no journal")
	}
	j, err := zs.OpenFileJournal(runJournalPath(t, opts))
	if err != nil {
		t.Fatalf("journal still held after Run returned: %v", err)
	}
	j.Close()
}

// On Solana, Run pays with the Solana keys, derives the payer from the lone
// one, and journals under the Solana deployment's key.
func TestRun_Solana(t *testing.T) {
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon " +
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"
	sk, err := keystore.NewSolanaKeys(context.Background(), mnemonicProvider{phrase})
	if err != nil {
		t.Fatal(err)
	}
	var got zs.Config
	var heldErr error
	cfg, opts := runConfig(t, nil)
	dir, _ := opts.stateDir()
	journal := filepath.Join(dir, "ticket-journal-solana-localnet-"+zsescrow.ProgramID.String()+".json")
	opts.buildClient = func(_ context.Context, zc zs.Config) (*zs.Client, error) {
		got = zc
		_, heldErr = zs.OpenFileJournal(journal)
		return nil, errors.New("rpc down")
	}
	opts.Keystore, opts.SolanaKeys = nil, sk
	cfg.Algod.Network, cfg.Zs.EscrowAppID = "", 0
	cfg.Solana.Cluster = "localnet"

	_ = Run(context.Background(), cfg, opts)
	if !errors.Is(heldErr, zs.ErrJournalInUse) {
		t.Errorf("opening the Solana journal during startup: %v, want ErrJournalInUse", heldErr)
	}
	if got.Signer != nil || got.SolanaSigner == nil || got.SolanaSigner.Address() != sk.Addresses()[0] {
		t.Errorf("Signer %v, SolanaSigner %v; want the lone Solana key %s", got.Signer, got.SolanaSigner, sk.Addresses()[0])
	}
	if cfg.Zs.ProxyPayerAddr != sk.Addresses()[0] {
		t.Errorf("payer %q, want %q", cfg.Zs.ProxyPayerAddr, sk.Addresses()[0])
	}
}

// The startup line names the RPC by scheme and host alone: a provider's key
// often rides in the URL's path or query.
func TestRun_SolanaLogsTheRPCHostOnly(t *testing.T) {
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon " +
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"
	sk, err := keystore.NewSolanaKeys(context.Background(), mnemonicProvider{phrase})
	if err != nil {
		t.Fatal(err)
	}
	cfg, opts := runConfig(t, func(_ context.Context, _ zs.Config) (*zs.Client, error) {
		return nil, errors.New("rpc down")
	})
	var logs bytes.Buffer
	opts.LogWriter = &logs
	opts.Keystore, opts.SolanaKeys = nil, sk
	cfg.Algod.Network, cfg.Zs.EscrowAppID = "", 0
	cfg.Solana.Cluster = "localnet"
	cfg.Solana.RPCEndpoint = "https://rpc.example:8899/v2/SECRETKEY?api-key=SECRETQUERY"

	runErr := Run(context.Background(), cfg, opts)
	if !strings.Contains(logs.String(), "https://rpc.example:8899") {
		t.Errorf("the RPC host isn't logged (Run: %v):\n%s", runErr, logs.String())
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Errorf("the RPC URL's path or query is logged:\n%s", logs.String())
	}
}

func TestEndpointHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://h.example:8899/k?x=1": "https://h.example:8899",
		"http://127.0.0.1:8899":        "http://127.0.0.1:8899",
		"::bad":                        "(unparsable)",
		"":                             "(unparsable)",
		"h.example/no-scheme":          "(unparsable)",
	} {
		if got := endpointHost(in); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// Run hands the SDK the default catalog path for its own deployment, in the
// same state dir as the journal, with the app-id name it had before.
func TestRun_DefaultCatalogIsPerDeployment(t *testing.T) {
	var got zs.Config
	cfg, opts := runConfig(t, func(_ context.Context, zc zs.Config) (*zs.Client, error) {
		got = zc
		return nil, errors.New("algod down")
	})
	cfg.Zs.CatalogPath = ""
	_ = Run(context.Background(), cfg, opts)
	dir, _ := opts.stateDir()
	if want := filepath.Join(dir, "operator-catalog-algorand-testnet-4242.json"); got.Discovery.CatalogPath != want {
		t.Errorf("catalog path = %q, want %q", got.Discovery.CatalogPath, want)
	}
	if want := filepath.Join(dir, "operator-catalog-4242.json"); got.Discovery.CatalogLegacyPath != want {
		t.Errorf("catalog legacy path = %q, want %q", got.Discovery.CatalogLegacyPath, want)
	}
}

// With no state dir given, Run journals in the proxy's own state dir.
func TestRun_JournalsInTheDefaultStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, home) {
		t.Fatalf("state dir %q is outside the test home %q", dir, home)
	}
	path := filepath.Join(dir, "ticket-journal-algorand-testnet-4242.json")
	var heldErr error
	cfg, opts := runConfig(t, func(context.Context, zs.Config) (*zs.Client, error) {
		_, heldErr = zs.OpenFileJournal(path)
		return nil, errors.New("algod down")
	})
	opts.stateDir = nil
	_ = Run(context.Background(), cfg, opts)
	if !errors.Is(heldErr, zs.ErrJournalInUse) {
		t.Errorf("opening %s during startup: %v, want ErrJournalInUse", path, heldErr)
	}
}

type closingClient struct{ log *[]string }

func (c closingClient) Close() { *c.log = append(*c.log, "client") }

type closingJournal struct{ log *[]string }

func (j closingJournal) Close() error                             { *j.log = append(*j.log, "journal"); return nil }
func (closingJournal) Put(context.Context, zs.TicketRecord) error { return nil }
func (closingJournal) Delete(context.Context, string) error       { return nil }
func (closingJournal) List(context.Context, string, zs.Deployment) ([]zs.TicketRecord, error) {
	return nil, nil
}

func TestCloseClientThenJournal(t *testing.T) {
	var got []string
	closeClientThenJournal(closingClient{&got}, closingJournal{&got})
	if len(got) != 2 || got[0] != "client" || got[1] != "journal" {
		t.Errorf("closed %v, want [client journal]", got)
	}
}

func failingBuild(context.Context, zs.Config) (*zs.Client, error) {
	return nil, errors.New("algod down")
}

// A failed start is an error, but a signal during it is a clean stop.
func TestRun_StartFailure(t *testing.T) {
	cfg, opts := runConfig(t, failingBuild)
	if err := Run(context.Background(), cfg, opts); err == nil || !strings.Contains(err.Error(), "algod down") {
		t.Errorf("live ctx: Run = %v, want the init failure", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, cfg, opts); err != nil {
		t.Errorf("canceled ctx: Run = %v, want a clean stop", err)
	}
}

// Windows `proxy stop` posts the token to /shutdown, and Run drains and
// returns.
func TestRun_ShutdownRouteStopsRun(t *testing.T) {
	client := stubClient(t)
	cfg, opts := runConfig(t, func(context.Context, zs.Config) (*zs.Client, error) { return client, nil })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg.Server.Listen = addr
	opts.ShutdownToken = "tok"

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg, opts) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/shutdown", nil)
		req.Header.Set("X-Shutdown-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST /shutdown: status %d, want 200", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run never served: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want a clean stop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run kept serving after POST /shutdown")
	}
	j, err := zs.OpenFileJournal(runJournalPath(t, opts))
	if err != nil {
		t.Fatalf("journal still held after Run returned: %v", err)
	}
	j.Close()
}

// A signal during New's chain reads is a stop, not a failed start; any other
// failure is.
func TestNewClient(t *testing.T) {
	failing := func(context.Context, zs.Config) (*zs.Client, error) { return nil, errors.New("algod down") }

	if c, err := newClient(context.Background(), zs.Config{}, failing); c != nil || err == nil || !strings.Contains(err.Error(), "algod down") {
		t.Errorf("live ctx: client %v err %v, want the init failure", c, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, err := newClient(ctx, zs.Config{}, failing); c != nil || err != nil {
		t.Errorf("canceled ctx: client %v err %v, want a clean stop", c, err)
	}

	want := stubClient(t)
	ok := func(context.Context, zs.Config) (*zs.Client, error) { return want, nil }
	if c, err := newClient(context.Background(), zs.Config{}, ok); c != want || err != nil {
		t.Errorf("success: client %v err %v, want the built client", c, err)
	}
}
