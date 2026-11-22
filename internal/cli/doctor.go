/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/app"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// diagnose is zs.Diagnose, apart so a test can supply the report.
var diagnose = zs.Diagnose

// doctorHints names the command that fixes a check, shown when it warns or
// fails. The SDK's messages are adapter-neutral; these are the proxy's own.
// prepaid_pool has no warn hint: its warnings range from a first request
// that hasn't deposited yet to every slot in use, and no one command fixes
// them all.
var doctorHints = map[string]map[zs.DoctorStatus]string{
	"algo":         {zs.DoctorFail: app.Remedies.Fund},
	"usdc_opt_in":  {zs.DoctorFail: "run `zs-proxy fund`, which opts in once ALGO has arrived"},
	"usdc_balance": {zs.DoctorWarn: app.Remedies.Fund},
	"prepaid_pool": {zs.DoctorFail: app.Remedies.Fund},
}

// solanaDoctorHints is doctorHints for zs.SolanaDoctorChecks. rent_pool has
// no warn hint, as prepaid_pool hasn't.
var solanaDoctorHints = map[string]map[zs.DoctorStatus]string{
	"sol":          {zs.DoctorWarn: app.SolanaRemedies.Fund},
	"usdc_account": {zs.DoctorFail: "run `zs-proxy fund`, which creates it"},
	"usdc_balance": {zs.DoctorWarn: app.SolanaRemedies.Fund},
	"rent_pool":    {zs.DoctorFail: app.SolanaRemedies.Fund},
}

// hintFor is the check's hint, or none when the check failed on a read: the
// account or pool couldn't be read, so funding it is not the fix. The SDK
// opens each read-failure message with "can't read" (doctor_vectors.json).
// A frozen token account is the mint's to thaw, which fund can't do.
func hintFor(c zs.DoctorCheck, onSolana bool) string {
	if strings.HasPrefix(c.Message, "can't read") || strings.Contains(c.Message, "is frozen") {
		return ""
	}
	if onSolana {
		return solanaDoctorHints[c.ID][c.Status]
	}
	return doctorHints[c.ID][c.Status]
}

var doctorMarks = map[zs.DoctorStatus]string{
	zs.DoctorPass: "✔",
	zs.DoctorWarn: "!",
	zs.DoctorFail: "✘",
	zs.DoctorSkip: "-",
}

func newDoctorCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Diagnose config, wallet, chain, funding and nodes",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			cfg, err := f.resolveConfig()
			check(out, err == nil, "config resolves", errMsg(err))
			if err != nil {
				return nil // can't proceed without config
			}
			if cfg.OnSolana() {
				program := "(preset)"
				if d, _, err := zs.ResolveNetwork(app.NetworkFor(cfg)); err == nil {
					program = d.Solana.Program.String()
				}
				fmt.Fprintf(out, "    network=%s  program=%s  listen=%s\n", cfg.NetworkName(), program, cfg.Server.Listen)
			} else {
				fmt.Fprintf(out, "    network=%s  escrow_app=%d  listen=%s\n",
					cfg.Algod.Network, cfg.Zs.EscrowAppID, cfg.Server.Listen)
			}

			exists := wallet.DefaultExists()
			check(out, exists, "wallet present", "run `zs-proxy wallet login`, `zs-proxy wallet import`, or `zs-proxy wallet new`")
			if !exists {
				return nil
			}
			meta, err := wallet.DefaultMetadata()
			if err != nil {
				check(out, false, "wallet metadata", errMsg(err))
				return nil
			}
			// Separate from the metadata: the chain checks below need only the
			// address, and still say something when the store does not open.
			_, err = wallet.OpenDefault()
			check(out, err == nil, "wallet store opens", errMsg(err))

			// The TEE verifier's config warnings belong in the doctor's
			// output; discovery's per-node logs don't, since the operators
			// check already counts the nodes it couldn't reach.
			sdkCfg := app.DoctorConfig(cfg, slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelWarn})))
			sdkCfg.Logger = slog.New(slog.DiscardHandler)
			address, err := payerAddress(cfg, meta)
			if err != nil {
				check(out, false, "payer address", errMsg(err))
				return nil
			}
			// A discovery cycle can take up to a minute.
			fmt.Fprintln(out, "    checking the chain and nodes…")
			renderDiagnosis(cmd.Context(), out, sdkCfg, address)

			check(out, portFree(cfg.Server.Listen), "listen port free", cfg.Server.Listen+" is in use")
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

// renderDiagnosis prints the SDK's chain, funding and node checks for address.
func renderDiagnosis(ctx context.Context, out io.Writer, cfg zs.Config, address string) {
	rep, err := diagnose(ctx, cfg, address)
	if err != nil {
		check(out, false, "chain checks", errMsg(err))
		return
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(out, "  ! %s\n", w)
	}
	for _, c := range rep.Checks {
		line := fmt.Sprintf("  %s %s: %s", doctorMarks[c.Status], c.ID, c.Message)
		if hint := hintFor(c, cfg.Network.Rail == ticket.RailSolana); hint != "" {
			line += " — " + hint
		}
		fmt.Fprintln(out, line)
	}
}
