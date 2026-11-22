/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// listenAddr strips the scheme so the httptest URL becomes a host:port the
// Record.Listen / controlBaseURL path expects.
func listenAddr(ts *httptest.Server) string {
	return strings.TrimPrefix(ts.URL, "http://")
}

func TestStopViaControlChannel(t *testing.T) {
	const token = "tok-123"

	t.Run("success POSTs token to /shutdown", func(t *testing.T) {
		var gotToken, gotMethod, gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotToken = r.Header.Get("X-Shutdown-Token")
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		rec := Record{Listen: listenAddr(ts), ShutdownToken: token}
		if err := stopViaControlChannel(rec); err != nil {
			t.Fatalf("stopViaControlChannel: %v", err)
		}
		if gotToken != token {
			t.Errorf("token = %q, want %q", gotToken, token)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/shutdown" {
			t.Errorf("path = %q, want /shutdown", gotPath)
		}
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer ts.Close()

		rec := Record{Listen: listenAddr(ts), ShutdownToken: token}
		if err := stopViaControlChannel(rec); err == nil {
			t.Fatal("expected an error on HTTP 403, got nil")
		}
	})

	t.Run("missing token errors before any request", func(t *testing.T) {
		rec := Record{Listen: "127.0.0.1:1", ShutdownToken: ""}
		if err := stopViaControlChannel(rec); err == nil {
			t.Fatal("expected an error for an empty shutdown token")
		}
	})
}

func TestControlBaseURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		":8080":          "http://127.0.0.1:8080",
	}
	for in, want := range cases {
		if got := controlBaseURL(in); got != want {
			t.Errorf("controlBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewShutdownToken(t *testing.T) {
	a, err := NewShutdownToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 { // 32 bytes hex-encoded
		t.Errorf("token length = %d, want 64 hex chars", len(a))
	}
	b, _ := NewShutdownToken()
	if a == b {
		t.Error("two tokens should not be identical")
	}
}
