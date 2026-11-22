/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestListener builds a listener without binding a socket — handleDeliver is
// a plain method, so we can exercise its guards directly.
func newTestListener(state string) *walletLoginListener {
	return &walletLoginListener{state: state, result: make(chan string, 1)}
}

func deliverRequest(t *testing.T, remoteAddr, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/deliver", strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestHandleDeliver_AcceptsMatchingState(t *testing.T) {
	const state = "deadbeefcafef00d"
	l := newTestListener(state)

	rec := httptest.NewRecorder()
	l.handleDeliver(rec, deliverRequest(t, "127.0.0.1:54321",
		`{"state":"`+state+`","phrase":"  abandon ability able  "}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case got := <-l.result:
		if got != "abandon ability able" { // trimmed
			t.Fatalf("delivered phrase = %q, want trimmed", got)
		}
	default:
		t.Fatal("phrase was not delivered on the result channel")
	}
}

func TestHandleDeliver_RejectsWrongState(t *testing.T) {
	l := newTestListener("the-real-nonce")

	rec := httptest.NewRecorder()
	l.handleDeliver(rec, deliverRequest(t, "127.0.0.1:54321",
		`{"state":"not-the-nonce","phrase":"abandon ability able"}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(l.result) != 0 {
		t.Fatal("phrase delivered despite a wrong state nonce")
	}
}

func TestHandleDeliver_RejectsNonLoopback(t *testing.T) {
	const state = "the-real-nonce"
	l := newTestListener(state)

	rec := httptest.NewRecorder()
	// A correct state must still be refused if the request is off-box.
	l.handleDeliver(rec, deliverRequest(t, "8.8.8.8:443",
		`{"state":"`+state+`","phrase":"abandon ability able"}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(l.result) != 0 {
		t.Fatal("phrase delivered from a non-loopback RemoteAddr")
	}
}

func TestHandleDeliver_RejectsEmptyPhrase(t *testing.T) {
	const state = "the-real-nonce"
	l := newTestListener(state)

	rec := httptest.NewRecorder()
	l.handleDeliver(rec, deliverRequest(t, "127.0.0.1:1",
		`{"state":"`+state+`","phrase":"   "}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(l.result) != 0 {
		t.Fatal("empty phrase delivered")
	}
}

func TestHandleDeliver_RejectsGet(t *testing.T) {
	l := newTestListener("nonce")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/deliver", nil)
	r.RemoteAddr = "127.0.0.1:1"
	l.handleDeliver(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestWalletLoginListener_RoundTrip exercises the real loopback socket: the
// served /cb page, then a /deliver POST over an actual 127.0.0.1 connection.
func TestWalletLoginListener_RoundTrip(t *testing.T) {
	l, err := startWalletLoginListener()
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer l.Close()

	base := fmt.Sprintf("http://127.0.0.1:%d", l.port)

	// The landing page is served and references the /deliver endpoint its inline
	// JS POSTs to.
	resp, err := http.Get(base + "/cb")
	if err != nil {
		t.Fatalf("GET /cb: %v", err)
	}
	pageBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /cb status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /cb content-type = %q, want text/html", ct)
	}
	if !strings.Contains(string(pageBytes), "/deliver") {
		t.Fatal("landing page does not reference /deliver")
	}

	// A correct delivery returns 200 and lands the phrase on the result channel.
	resp, err = http.Post(base+"/deliver", "application/json",
		strings.NewReader(`{"state":"`+l.state+`","phrase":"alpha bravo charlie"}`))
	if err != nil {
		t.Fatalf("POST /deliver: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /deliver status = %d, want 200", resp.StatusCode)
	}
	select {
	case got := <-l.result:
		if got != "alpha bravo charlie" {
			t.Fatalf("delivered phrase = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delivered phrase")
	}
}

func TestBuildClientLoginURL(t *testing.T) {
	got, err := buildClientLoginURL("https://zerosignal.ai", 5050, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://zerosignal.ai/cli?port=5050&state=abc123" {
		t.Fatalf("url = %q", got)
	}

	// Trailing slash on the base must not double up the /cli path.
	got, err = buildClientLoginURL("http://localhost:3000/", 80, "n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://localhost:3000/cli?port=80&state=n" {
		t.Fatalf("url with trailing slash = %q", got)
	}

	if _, err := buildClientLoginURL("not a url", 1, "n"); err == nil {
		t.Fatal("expected an error for a hostless URL")
	}
}
