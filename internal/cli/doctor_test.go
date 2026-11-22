/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/app"
)

func stubDiagnose(t *testing.T, fn func(context.Context, zs.Config, string) (zs.DoctorReport, error)) {
	t.Helper()
	prev := diagnose
	diagnose = fn
	t.Cleanup(func() { diagnose = prev })
}

func TestRenderDiagnosis_EachStatusAndItsHint(t *testing.T) {
	var gotCfg zs.Config
	var gotAddr string
	stubDiagnose(t, func(_ context.Context, cfg zs.Config, addr string) (zs.DoctorReport, error) {
		gotCfg, gotAddr = cfg, addr
		return zs.DoctorReport{
			Warnings: []string{"a config warning"},
			Checks: []zs.DoctorCheck{
				{ID: "algod", Status: zs.DoctorPass, Message: "algod answers"},
				{ID: "algo", Status: zs.DoctorFail, Message: "holds no ALGO"},
				{ID: "usdc_opt_in", Status: zs.DoctorSkip, Message: "needs the account"},
				{ID: "prepaid_pool", Status: zs.DoctorFail, Message: "the pool backs no ticket"},
				{ID: "usdc_balance", Status: zs.DoctorWarn, Message: "holds no USDC"},
			},
		}, nil
	})

	var out bytes.Buffer
	renderDiagnosis(context.Background(), &out, zs.Config{ConcurrentSlots: 7}, "ADDR")

	if gotAddr != "ADDR" || gotCfg.ConcurrentSlots != 7 {
		t.Errorf("diagnose got (%+v, %q), want the config and address it was handed", gotCfg.ConcurrentSlots, gotAddr)
	}
	want := strings.Join([]string{
		"  ! a config warning",
		"  ✔ algod: algod answers",
		"  ✘ algo: holds no ALGO — " + app.Remedies.Fund,
		"  - usdc_opt_in: needs the account",
		"  ✘ prepaid_pool: the pool backs no ticket — " + app.Remedies.Fund,
		"  ! usdc_balance: holds no USDC — " + app.Remedies.Fund,
		"",
	}, "\n")
	if out.String() != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestRenderDiagnosis_ErrorIsAFailedLine(t *testing.T) {
	stubDiagnose(t, func(context.Context, zs.Config, string) (zs.DoctorReport, error) {
		return zs.DoctorReport{}, errors.New("address is not an Algorand address")
	})
	var out bytes.Buffer
	renderDiagnosis(context.Background(), &out, zs.Config{}, "x")
	if got, want := out.String(), "  ✘ chain checks — address is not an Algorand address\n"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

// A failed read is not a funding problem, so it gets no funding hint.
func TestRenderDiagnosis_NoHintOnAFailedRead(t *testing.T) {
	stubDiagnose(t, func(context.Context, zs.Config, string) (zs.DoctorReport, error) {
		return zs.DoctorReport{Checks: []zs.DoctorCheck{
			{ID: "algo", Status: zs.DoctorFail, Message: "can't read ADDR: 503"},
			{ID: "prepaid_pool", Status: zs.DoctorFail, Message: "can't read the prepaid ticket pool: 503"},
		}}, nil
	})
	var out bytes.Buffer
	renderDiagnosis(context.Background(), &out, zs.Config{}, "ADDR")
	want := "  ✘ algo: can't read ADDR: 503\n  ✘ prepaid_pool: can't read the prepaid ticket pool: 503\n"
	if out.String() != want {
		t.Errorf("rendered %q, want %q", out.String(), want)
	}
}

// A hint keyed to a check the SDK doesn't report is dead advice, and only a
// warning or failure shows one.
func TestDoctorHints_KeyedToRealChecks(t *testing.T) {
	for _, rail := range []struct {
		name  string
		hints map[string]map[zs.DoctorStatus]string
		ids   []zs.DoctorCheck
	}{
		{"doctorHints", doctorHints, zs.DoctorChecks(zs.DoctorReadings{})},
		{"solanaDoctorHints", solanaDoctorHints, zs.SolanaDoctorChecks(zs.SolanaDoctorReadings{})},
	} {
		known := map[string]bool{}
		for _, c := range rail.ids {
			known[c.ID] = true
		}
		for id, byStatus := range rail.hints {
			if !known[id] {
				t.Errorf("%s names check %q, which the SDK doesn't report on that rail", rail.name, id)
			}
			for status := range byStatus {
				if status != zs.DoctorWarn && status != zs.DoctorFail {
					t.Errorf("%s[%s] has a hint for %q; only a warning or failure shows one", rail.name, id, status)
				}
			}
		}
	}
}

// Each rail's checks get that rail's hints: usdc_balance is on both, and a
// Solana payer is told to send SOL, not ALGO.
func TestHintFor_ByRail(t *testing.T) {
	low := zs.DoctorCheck{ID: "usdc_balance", Status: zs.DoctorWarn, Message: "holds no USDC"}
	if h := hintFor(low, true); !strings.Contains(h, "SOL") || strings.Contains(h, "ALGO") {
		t.Errorf("Solana hint %q, want SOL", h)
	}
	if h := hintFor(low, false); !strings.Contains(h, "ALGO") {
		t.Errorf("Algorand hint %q, want ALGO", h)
	}
	// The SDK's own messages, so a reworded one fails here rather than
	// bringing the fund hint back to a frozen account.
	usdcAccount := func(exists, frozen bool) zs.DoctorCheck {
		t.Helper()
		for _, c := range zs.SolanaDoctorChecks(zs.SolanaDoctorReadings{TicketRent: 1, UsdcAccount: exists, UsdcFrozen: frozen}) {
			if c.ID == "usdc_account" {
				if c.Status != zs.DoctorFail {
					t.Fatalf("usdc_account %s: %s", c.Status, c.Message)
				}
				return c
			}
		}
		t.Fatal("no usdc_account check")
		return zs.DoctorCheck{}
	}
	frozen, missing := usdcAccount(true, true), usdcAccount(false, false)
	if h := hintFor(frozen, true); h != "" {
		t.Errorf("frozen account hint %q, want none", h)
	}
	if h := hintFor(missing, true); !strings.Contains(h, "`zs-proxy fund`") {
		t.Errorf("missing account hint %q, want fund", h)
	}

	// A pool that can't fund an open is fund's to fix; a warn on it, which
	// funding can't fix, gets no hint, as Algorand's prepaid_pool doesn't.
	if h := hintFor(zs.DoctorCheck{ID: "rent_pool", Status: zs.DoctorFail}, true); h != app.SolanaRemedies.Fund {
		t.Errorf("rent_pool fail hint %q", h)
	}
	if h := hintFor(zs.DoctorCheck{ID: "rent_pool", Status: zs.DoctorWarn}, true); h != "" {
		t.Errorf("rent_pool warn hint %q, want none", h)
	}
	if h := hintFor(zs.DoctorCheck{ID: "sol", Status: zs.DoctorWarn}, true); h != app.SolanaRemedies.Fund {
		t.Errorf("sol warn hint %q", h)
	}
}
