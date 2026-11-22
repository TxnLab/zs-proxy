/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
)

// withShutdownRoute adds the loopback-only graceful-stop control channel in
// front of next. It exists for Windows, which has no SIGTERM: `proxy stop`
// POSTs here with the token from daemon.json, and the route triggers the same
// graceful drain a SIGTERM does on unix. An empty token returns next
// unchanged: foreground and service-managed runs expose no such route.
//
// Two guards: the request must originate on the loopback interface (it can't
// come from off-box even if the proxy binds a public address), and it must carry
// the matching X-Shutdown-Token. The token is the real guard: a local web page
// can reach 127.0.0.1 too. It lives in daemon.json, readable only by the user
// who owns the process — consistent with the single-user-localhost model. This
// is a control-plane endpoint with no request/response content, so it does not
// touch the non-retention rule.
func withShutdownRoute(next http.Handler, token string, shutdown func(), log *slog.Logger) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/shutdown" {
			next.ServeHTTP(w, r)
			return
		}
		// Refusals are logged here because this route sits outside the
		// handler's request log: a local page probing it should leave a trace.
		if !IsLoopbackRequest(r) {
			log.Warn("shutdown request refused", slog.String("reason", "not loopback"), slog.String("remote", r.RemoteAddr))
			http.Error(w, "shutdown is loopback-only", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Shutdown-Token")), []byte(token)) != 1 {
			log.Warn("shutdown request refused", slog.String("reason", "bad token"))
			http.Error(w, "invalid shutdown token", http.StatusForbidden)
			return
		}

		log.Info("graceful shutdown requested via control channel")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"stopping"}` + "\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// After the response is on the wire, and asynchronously, so this
		// handler returns and frees its connection for srv.Shutdown to drain.
		go shutdown()
	})
}

// IsLoopbackRequest reports whether the request originated from the loopback
// interface (127.0.0.0/8 or ::1). Exported for the wallet-login callback,
// which has the same guard.
func IsLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
