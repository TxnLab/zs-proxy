/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/app"
	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/daemon"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// walletLoginTimeout bounds how long `wallet login` waits for the browser to
// hand the phrase back before giving up. Generous enough for a passkey ceremony
// (biometric prompt, possibly a fresh sign-in first), short enough not to wedge
// a terminal forever.
const walletLoginTimeout = 3 * time.Minute

// newWalletLoginCmd is `zs-proxy wallet login`: the npm/gh-login-shaped browser
// handoff that imports the same account the client derives, without revealing or
// pasting the recovery phrase. It opens the hosted client to a one-time consent
// page, the user authenticates with their passkey, and the 24-word phrase is
// delivered back over a one-shot loopback listener (see walletLoginListener).
//
// The phrase never transits a remote server: it rides the URL *fragment*
// (#…, never sent in the request line) on a top-level nav to 127.0.0.1, then a
// same-origin loopback POST. Everything downstream — derive, confirm, Import — is
// byte-identical to `wallet import`; only the transport differs.
func newWalletLoginCmd() *cobra.Command {
	var (
		network   string
		force     bool
		assumeYes bool
		clientURL string
		noBrowser bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in with your browser to import the client wallet (no paste)",
		Long: "Import your wallet by signing in to the zerosignal web client with your\n" +
			"passkey, instead of pasting the 24-word recovery phrase. `wallet login`\n" +
			"opens the client, you approve the handoff, and the phrase is delivered\n" +
			"back over a one-shot localhost listener — it never touches a remote\n" +
			"server. The derived accounts are identical to `wallet import`'s, so\n" +
			"the CLI and the browser app share one account.\n\n" +
			"Use --no-browser on a headless/SSH host to print the URL instead of\n" +
			"opening it. `wallet import` remains the paste fallback for airgapped use.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := wallet.OpenDefault()
			if err != nil {
				return err
			}
			if w.Exists() && !force {
				return wallet.ErrExists
			}

			phrase, err := walletLoginPhrase(cmd, clientURL, noBrowser)
			if err != nil {
				return err
			}
			// The consent screen showed the browser's own rail's address, which
			// may not be this network's, so the CLI confirms it again.
			return importPhrase(cmd, w, phrase, network, force, assumeYes, "login aborted — address not confirmed")
		},
	}
	cmd.Flags().StringVar(&network, "network", build.DefaultNetwork, "network this wallet is for")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing wallet")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip the address-match confirmation")
	cmd.Flags().StringVar(&clientURL, "client-url", build.ClientURL, "base URL of the hosted client to sign in to")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser (headless/SSH)")
	return cmd
}

// walletLoginPhrase is runWalletLogin, a var so tests can hand the login
// commands a phrase without a browser.
var walletLoginPhrase = runWalletLogin

// runWalletLogin spins up the one-shot loopback listener, opens (or prints) the
// client sign-in URL, and blocks until the browser delivers the phrase, the
// timeout elapses, or the user cancels (Ctrl-C). The returned string is the only
// copy of the phrase and must never be logged — it flows straight into Import.
func runWalletLogin(cmd *cobra.Command, clientURL string, noBrowser bool) (string, error) {
	lst, err := startWalletLoginListener()
	if err != nil {
		return "", err
	}
	defer lst.Close()

	signinURL, err := buildClientLoginURL(clientURL, lst.port, lst.state)
	if err != nil {
		return "", err
	}

	out := cmd.OutOrStdout()
	if noBrowser {
		fmt.Fprintf(out, "Open this URL in a browser already signed in to your client:\n\n  %s\n\n", signinURL)
	} else {
		fmt.Fprintf(out, "Opening your browser to sign in…\n  %s\n", signinURL)
		if err := browser.OpenURL(signinURL); err != nil {
			fmt.Fprintln(out, "(couldn't open a browser automatically — open the URL above manually)")
		}
	}
	fmt.Fprintln(out, "Waiting for the browser handoff… (Ctrl-C to cancel)")

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case phrase := <-lst.result:
		return phrase, nil
	case <-time.After(walletLoginTimeout):
		return "", errors.New("timed out waiting for the browser — re-run `zs-proxy wallet login`")
	case <-ctx.Done():
		return "", errors.New("login cancelled")
	}
}

// buildClientLoginURL renders https://<client>/cli?port=<port>&state=<nonce> from
// the configured base, preserving any path prefix the base carries.
func buildClientLoginURL(base string, port int, state string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid --client-url %q: want a URL like https://zerosignal.ai", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/cli"
	q := u.Query()
	q.Set("port", strconv.Itoa(port))
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// walletLoginListener is the one-shot, loopback-only HTTP server that receives
// the recovery phrase from the browser. It lives only for the duration of the
// `wallet login` command — it deliberately does NOT piggyback the main proxy mux
// (the proxy may not be running, and this is a command-lifecycle concern).
type walletLoginListener struct {
	state  string      // the per-launch nonce the page must echo back on /deliver
	result chan string // buffered(1): the delivered phrase
	port   int
	ln     net.Listener
	srv    *http.Server
}

// startWalletLoginListener binds an ephemeral loopback port, mints a 256-bit
// state nonce, and starts serving the two-route handoff (/cb + /deliver).
func startWalletLoginListener() (*walletLoginListener, error) {
	state, err := daemon.NewShutdownToken() // 256-bit crypto/rand hex — reused as the handoff nonce
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open loopback listener: %w", err)
	}
	l := &walletLoginListener{
		state:  state,
		result: make(chan string, 1),
		port:   ln.Addr().(*net.TCPAddr).Port,
		ln:     ln,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cb", l.handleCallback)
	mux.HandleFunc("/deliver", l.handleDeliver)
	l.srv = &http.Server{Handler: mux}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// Close shuts the listener down. Safe to call once the result is read (or on the
// error/timeout/cancel paths, via defer).
func (l *walletLoginListener) Close() error { return l.srv.Close() }

// handleCallback serves the landing page the browser top-level-navigates to
// (http://127.0.0.1:<port>/cb#state=…&phrase=…). Its inline JS reads the
// fragment, wipes it from history, and POSTs it same-origin to /deliver — the
// fragment never reaches this handler (fragments are not sent in the request).
func (l *walletLoginListener) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !app.IsLoopbackRequest(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, walletLoginPage)
}

// handleDeliver is the receiver for the phrase. Guards, in order:
//   - loopback-only RemoteAddr (can't be reached off-box);
//   - constant-time state-nonce match — this, not loopback, is the real guard:
//     any local page can reach 127.0.0.1 with a loopback RemoteAddr (the
//     shutdown.go lesson), so the per-launch nonce is what authenticates the
//     page to this CLI.
//
// On success the phrase is pushed to the buffered result channel and RunE
// derives/confirms/imports it. The phrase, the request body, and the fragment
// are NEVER logged (proxy non-retention rule).
func (l *walletLoginListener) handleDeliver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !app.IsLoopbackRequest(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	var body struct {
		State  string `json:"state"`
		Phrase string `json:"phrase"`
	}
	// Cap the body — a recovery phrase is well under 1KB; refuse anything large.
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.State), []byte(l.state)) != 1 {
		http.Error(w, "invalid state", http.StatusForbidden)
		return
	}
	phrase := strings.TrimSpace(body.Phrase)
	if phrase == "" {
		http.Error(w, "empty phrase", http.StatusBadRequest)
		return
	}

	// Write + flush the success response to the browser FIRST, then hand the
	// phrase to RunE. RunE closes the listener as soon as it reads the result, so
	// signaling before the response is on the wire would race the page into a
	// spurious "couldn't connect" (same ordering lesson as handleShutdown).
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Single-use: the first valid delivery fills the buffered channel; a duplicate
	// POST (e.g. a reload) is acknowledged but ignored rather than re-delivered.
	select {
	case l.result <- phrase:
	default:
	}
}

// walletLoginPage is the minimal landing page served at /cb. Its inline JS reads
// the URL fragment (state + phrase), immediately strips it from the address bar
// /history, and POSTs it same-origin to /deliver — so the phrase only ever exists
// in browser memory and a loopback POST body. It never logs or persists anything.
const walletLoginPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>zs-proxy — sign in</title>
<style>
  :root { color-scheme: dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
    font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif; background:#0b0b0c; color:#e7e7ea; }
  .card { max-width:30rem; padding:2.5rem; text-align:center; }
  h1 { font-size:1.4rem; margin:0 0 .5rem; }
  p { margin:.25rem 0; color:#a7a7ad; }
  .ok { color:#4ade80; } .err { color:#f87171; }
  code { display:block; margin-top:1rem; font-size:.8rem; opacity:.55; word-break:break-all; }
</style>
</head>
<body>
<div class="card">
  <h1 id="title">Finishing sign-in…</h1>
  <p id="msg">Delivering your wallet to zs-proxy on this machine.</p>
</div>
<script>
(function () {
  var title = document.getElementById('title'), msg = document.getElementById('msg');
  var params = new URLSearchParams(location.hash.replace(/^#/, ''));
  var state = params.get('state'), phrase = params.get('phrase');
  // Wipe the fragment from the address bar + history so the phrase doesn't linger.
  history.replaceState(null, '', location.pathname);
  function fail(text, detail) {
    title.textContent = 'Couldn’t connect'; title.className = 'err'; msg.textContent = text;
    if (detail) { var c = document.createElement('code'); c.textContent = detail; document.querySelector('.card').appendChild(c); }
  }
  if (!state || !phrase) { fail('This page was opened without a handoff payload. Re-run the command in your terminal.'); return; }
  fetch('/deliver', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ state: state, phrase: phrase })
  }).then(function (r) {
    if (r.ok) {
      title.textContent = '✓ Sent to zs-proxy'; title.className = 'ok';
      msg.textContent = 'Return to your terminal to confirm the address and finish. You can close this tab.';
    } else {
      return r.text().then(function (t) { throw new Error(t.trim() || ('HTTP ' + r.status)); });
    }
  }).catch(function (e) { fail('The CLI rejected the handoff. Close this tab and try again.', e && e.message); });
})();
</script>
</body>
</html>
`
