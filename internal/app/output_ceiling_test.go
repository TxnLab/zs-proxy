/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/selection"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// Any fallback ceiling other than the shared default makes this proxy disagree
// with the chat client about which operators can serve a request, since
// eligibility is evaluated against the DERIVED number — and nothing else
// surfaces that. Below the default additionally reintroduces the blank-bubble
// truncation (the derivation used to floor the ceiling at 32768 for reasoning
// models no matter what was configured here; that floor is gone now that there
// is a single ceiling), and above it locks more escrow per request than needed.
func TestWarnOutputCeilingDivergence(t *testing.T) {
	const (
		warnNone = ""
		warnLow  = "BELOW the shared default"
		warnHigh = "ABOVE the shared default"
	)
	cases := []struct {
		name    string
		ceiling uint64
		want    string
	}{
		{
			// The value proxy/config.example.yaml shipped, so a real operator's
			// copied config lands exactly here.
			name: "the old example value warns low", ceiling: 8192, want: warnLow,
		},
		{name: "just below the default warns low", ceiling: selection.DefaultMaxOutputCeiling - 1, want: warnLow},
		{name: "the previous proxy default warns low", ceiling: 32000, want: warnLow},
		{name: "the shared default is quiet", ceiling: selection.DefaultMaxOutputCeiling, want: warnNone},
		// The routing-parity argument is symmetric — a ceiling above the default
		// picks a different operator set for the same request just as surely as
		// one below it, and the client has no matching knob to follow it with.
		{name: "just above the default warns high", ceiling: selection.DefaultMaxOutputCeiling + 1, want: warnHigh},
		{name: "well above the default warns high", ceiling: 131072, want: warnHigh},
		{
			// 0 is the deliberate fail-closed setting (reject omitting callers
			// with max_output_required), not a diverging ceiling.
			name: "fail-closed zero is quiet", ceiling: 0, want: warnNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := &config.Config{}
			cfg.Zs.FallbackMaxOutputTokens = tc.ceiling
			warnOutputCeilingDivergence(cfg, newLoggerTo(config.LoggingConfig{Level: "info", Format: "text"}, &buf))
			out := buf.String()

			// Assert on both directions every time, so a warning that fires with
			// the wrong wording fails rather than passing on a substring hit.
			for _, phrase := range []string{warnLow, warnHigh} {
				if got, want := strings.Contains(out, phrase), tc.want == phrase; got != want {
					t.Errorf("contains %q = %v, want %v\n--- log ---\n%s", phrase, got, want, out)
				}
			}
			if tc.want != warnNone && !strings.Contains(out, "level=WARN") {
				t.Errorf("expected WARN level, got:\n%s", out)
			}
		})
	}
}
