/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/escrow"
)

// ticketMbrFixture is a representative mbrForTicket() value (~0.115 ALGO).
const ticketMbrFixture = 115_700

func TestPoolLine(t *testing.T) {
	tests := []struct {
		name      string
		dep       escrow.PayerDeposit
		ticketMbr uint64
		want      string
	}{
		{
			// RefreshConfig didn't load the box MBR: no denominator is knowable,
			// so the line stays on the bare count rather than dividing by zero.
			name:      "unknown ticket MBR falls back to the open count",
			dep:       escrow.PayerDeposit{AlgoBalance: 1_157_000, OpenTickets: 2},
			ticketMbr: 0,
			want:      "MBR pool: 1.157000 ALGO deposited, 2 ticket(s) open",
		},
		{
			name:      "idle pool at target",
			dep:       escrow.PayerDeposit{AlgoBalance: 10 * ticketMbrFixture, OpenTickets: 0},
			ticketMbr: ticketMbrFixture,
			want:      "MBR pool: 1.157000 ALGO deposited, 0 of 10 ticket slot(s) in use",
		},
		{
			// Under the configured working set: the denominator reports what the
			// deposit actually backs, which is what open() admits against.
			name:      "under-funded pool reports the slots it really backs",
			dep:       escrow.PayerDeposit{AlgoBalance: 8 * ticketMbrFixture, OpenTickets: 2},
			ticketMbr: ticketMbrFixture,
			want:      "MBR pool: 0.925600 ALGO deposited, 2 of 8 ticket slot(s) in use",
		},
		{
			// The free-quota box term is a permanent sub-slot debit, so the
			// balance carries a remainder. Floor, never round up: the partial
			// slot cannot back a ticket.
			name:      "sub-slot remainder does not round up",
			dep:       escrow.PayerDeposit{AlgoBalance: 10*ticketMbrFixture - 22_500, OpenTickets: 1},
			ticketMbr: ticketMbrFixture,
			want:      "MBR pool: 1.134500 ALGO deposited, 1 of 9 ticket slot(s) in use",
		},
		{
			// Defensive: the contract's pool invariant makes this unreachable,
			// but a denominator below the numerator would be nonsense.
			name:      "open count never exceeds the denominator",
			dep:       escrow.PayerDeposit{AlgoBalance: ticketMbrFixture, OpenTickets: 3},
			ticketMbr: ticketMbrFixture,
			want:      "MBR pool: 0.115700 ALGO deposited, 3 of 3 ticket slot(s) in use",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolLine(tc.dep, tc.ticketMbr); got != tc.want {
				t.Fatalf("poolLine() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestFreeQuotaLine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resets := now.Add(6 * time.Hour)

	tests := []struct {
		name       string
		al         escrow.FreeAllowance
		boxPending bool
		want       string
	}{
		{
			// A cap of 0 can only mean a contract this build doesn't understand.
			name: "no cap says nothing",
			al:   escrow.FreeAllowance{Remaining: 0, CapPerDay: 0},
			want: "",
		},
		{
			// Full allowance: no window is running, so WindowEndsAt is the end of
			// one that would only start on the next free open — not a deadline.
			name:       "full allowance with no box yet",
			al:         escrow.FreeAllowance{Remaining: 20, WindowEndsAt: uint64(resets.Unix()), CapPerDay: 20},
			boxPending: true,
			want:       "free:     20 of 20 free tickets available today (quota box not yet created)",
		},
		{
			name: "full allowance after the window lapsed",
			al:   escrow.FreeAllowance{Remaining: 20, WindowEndsAt: uint64(resets.Unix()), CapPerDay: 20},
			want: "free:     20 of 20 free tickets available today",
		},
		{
			name: "partial allowance names the reset",
			al:   escrow.FreeAllowance{Remaining: 18, WindowEndsAt: uint64(resets.Unix()), CapPerDay: 20},
			want: "free:     18 of 20 free tickets left today (window resets " +
				resets.Format(time.RFC3339) + ", in 6h0m0s)",
		},
		{
			name: "exhausted allowance",
			al:   escrow.FreeAllowance{Remaining: 0, WindowEndsAt: uint64(resets.Unix()), CapPerDay: 20},
			want: "free:     0 of 20 free tickets left today (window resets " +
				resets.Format(time.RFC3339) + ", in 6h0m0s)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := freeQuotaLine(tc.al, tc.boxPending, now); got != tc.want {
				t.Fatalf("freeQuotaLine() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}
