/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	server "github.com/TxnLab/zerosignal/go/zs/handler"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// served answers every request it reaches with 418, so a test can tell the
// control route from a fall-through.
var served = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

func postShutdown(h http.Handler, remoteAddr, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set("X-Shutdown-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestShutdownRoute_Valid(t *testing.T) {
	called := make(chan struct{}, 1)
	h := withShutdownRoute(served, "secret", func() { called <- struct{}{} }, quietLogger())

	if rec := postShutdown(h, "127.0.0.1:54321", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown callback was not invoked")
	}
}

func TestShutdownRoute_Rejects(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		token      string
	}{
		{"wrong token", "127.0.0.1:5", "nope"},
		{"missing token", "127.0.0.1:5", ""},
		{"non-loopback caller", "203.0.113.7:5", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired := make(chan struct{}, 1)
			h := withShutdownRoute(served, "secret", func() { fired <- struct{}{} }, quietLogger())

			if rec := postShutdown(h, tc.remoteAddr, tc.token); rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			// A rejected request must never reach the trigger.
			time.Sleep(50 * time.Millisecond)
			select {
			case <-fired:
				t.Fatal("shutdown callback fired on a rejected request")
			default:
			}
		})
	}
}

// A foreground or service-managed run (no token) exposes no route, and every
// other request reaches the proxy's own handler.
func TestShutdownRoute_OnlyWithATokenAndOnlyItsRoute(t *testing.T) {
	fired := false
	trigger := func() { fired = true }
	if rec := postShutdown(withShutdownRoute(served, "", trigger, quietLogger()), "127.0.0.1:5", ""); rec.Code != http.StatusTeapot {
		t.Errorf("no token: status = %d, want the request served", rec.Code)
	}
	h := withShutdownRoute(served, "secret", trigger, quietLogger())
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/shutdown", nil),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
	} {
		req.RemoteAddr = "127.0.0.1:5"
		req.Header.Set("X-Shutdown-Token", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s %s: status = %d, want the request served", req.Method, req.URL.Path, rec.Code)
		}
	}
	if fired {
		t.Error("a request other than POST /shutdown fired the trigger")
	}
}

// fakeHandler stands in for the SDK handler's HTTPServer.
type fakeHandler struct{}

func (fakeHandler) HTTPServer(lc server.ListenConfig) *http.Server {
	return &http.Server{Addr: lc.Addr, Handler: served}
}

// The server Run serves: a real loopback POST with the token reaches the route
// and drives a graceful srv.Shutdown, and everything else reaches the handler.
func TestHTTPServerFor_EndToEnd(t *testing.T) {
	const token = "integration-tok"
	stopped := make(chan struct{})
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:9999"
	var hs *http.Server
	hs = httpServerFor(fakeHandler{}, cfg, token, func() { go func() { _ = hs.Shutdown(context.Background()) }() }, quietLogger())
	if hs.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want the configured listen address", hs.Addr)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = hs.Serve(ln) // returns ErrServerClosed once Shutdown runs
		close(stopped)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	other, err := client.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = other.Body.Close()
	if other.StatusCode != http.StatusTeapot {
		t.Errorf("GET /healthz: status = %d, want the handler's", other.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/shutdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Shutdown-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /shutdown: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down after POST /shutdown")
	}
}

func TestIsLoopbackRequest(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"127.0.0.1":      true, // no port → host is the whole string
		"203.0.113.7:9":  false,
		"10.0.0.5:9":     false,
	}
	for addr, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
		req.RemoteAddr = addr
		if got := IsLoopbackRequest(req); got != want {
			t.Errorf("IsLoopbackRequest(%q) = %v, want %v", addr, got, want)
		}
	}
}
