/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newConfigCmd is the `zs-proxy config` inspection group.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration",
	}
	cmd.AddCommand(newConfigPathCmd(), newConfigPrintEffectiveCmd())
	return cmd
}

// newConfigPathCmd prints which config file (if any) would be loaded.
func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "path",
		Short:        "Print the config file path that would be loaded",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if p := defaultConfigPath(); p != "" {
				fmt.Fprintln(cmd.OutOrStdout(), p)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "(none — using embedded defaults)")
			}
			return nil
		},
	}
}

// newConfigPrintEffectiveCmd dumps the merged config (embedded defaults +
// file + env + flags) as YAML, using the same resolution path proxy start
// runs — so the output is exactly what would boot.
func newConfigPrintEffectiveCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "print-effective",
		Short:        "Print the merged effective config (defaults + file + env + flags)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := f.resolveConfig()
			if err != nil {
				return err
			}
			out, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("marshal config: %w", err)
			}
			src := f.configPath
			if src == "" {
				src = "(none — embedded defaults)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# effective config (file: %s, network: %s)\n%s", src, cfg.NetworkName(), out)
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}
