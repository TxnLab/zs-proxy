/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TxnLab/zs-proxy/internal/config"
)

func proxyAt(raw string) func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) {
		if raw == "" {
			return nil, nil
		}
		return url.Parse(raw)
	}
}

func envProxyLog(t *testing.T, raw string, allowPrivate bool) string {
	t.Helper()
	var buf bytes.Buffer
	warnEnvProxy(newLoggerTo(config.LoggingConfig{Level: "info", Format: "text"}, &buf), allowPrivate, proxyAt(raw))
	return buf.String()
}

// A proxy URL's username is often an API key, and this line lands in a
// persistent daemon log; url.Redacted would keep it.
func TestWarnEnvProxy_LogsNoCredentials(t *testing.T) {
	out := envProxyLog(t, "http://apikey123:pw456@proxy.example:3128", false)
	if strings.Contains(out, "apikey123") || strings.Contains(out, "pw456") {
		t.Errorf("credentials logged:\n%s", out)
	}
	if !strings.Contains(out, "proxy=http://proxy.example:3128") {
		t.Errorf("proxy host not named:\n%s", out)
	}
}

// One proxy serving both schemes is one line.
func TestWarnEnvProxy_OneLinePerProxy(t *testing.T) {
	if n := strings.Count(envProxyLog(t, "http://proxy.example:3128", false), "\n"); n != 1 {
		t.Errorf("%d log lines, want 1", n)
	}
	if out := envProxyLog(t, "", false); out != "" {
		t.Errorf("no proxy set, logged:\n%s", out)
	}
}

// With the guard on, a proxy on a private address is itself refused, so every
// node request fails: that is an error, not a caveat.
func TestWarnEnvProxy_PrivateProxyIsAnErrorUnlessAllowed(t *testing.T) {
	for _, tc := range []struct {
		proxy        string
		allowPrivate bool
		want         string
	}{
		{"http://127.0.0.1:3128", false, "level=ERROR"},
		{"http://localhost:3128", false, "level=ERROR"},
		{"http://10.1.2.3:3128", false, "level=ERROR"},
		{"http://10.1.2.3:3128", true, "level=WARN"},
		{"http://203.0.113.9:3128", false, "level=WARN"},
	} {
		if out := envProxyLog(t, tc.proxy, tc.allowPrivate); !strings.Contains(out, tc.want) {
			t.Errorf("%s allowPrivate=%v: want %s in\n%s", tc.proxy, tc.allowPrivate, tc.want, out)
		}
	}
}
