/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/wire"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/build"
)

// newVersionCmd prints the three independently-versioned axes: the product
// version, the wire proto version, and the embedded per-network escrow app ids
// (the real compatibility surface — see the plan's versioning notes).
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "version",
		Short:        "Print version, proto version, and embedded network ids",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "zs-proxy %s (%s)\n", build.Version, build.Commit)
			fmt.Fprintf(out, "proto:    %s\n", wire.ProtoVersion)

			tn, _ := build.NetworkPreset("testnet")
			fmt.Fprintf(out, "testnet escrow app: %d\n", tn.EscrowAppID)
			if mn, _ := build.NetworkPreset("mainnet"); mn.EscrowAppID != 0 {
				fmt.Fprintf(out, "mainnet escrow app: %d\n", mn.EscrowAppID)
			} else {
				fmt.Fprintln(out, "mainnet escrow app: (not deployed)")
			}
			for _, c := range zs.SolanaClusters() {
				if _, program, ok := zs.SolanaClusterPreset(c); ok {
					fmt.Fprintf(out, "solana-%s program: %s\n", c, program)
				}
			}
			return nil
		},
	}
}
