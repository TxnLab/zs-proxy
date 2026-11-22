/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import "github.com/spf13/cobra"

// newProxyCmd is the `zs-proxy proxy` command group: the daemon lifecycle
// (start/stop/status/restart/logs) plus install-service/uninstall-service for
// OS-service registration (launchd / systemd user units / Windows SCM).
func newProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Manage the proxy daemon",
	}
	cmd.AddCommand(
		newProxyStartCmd(),
		newProxyStopCmd(),
		newProxyStatusCmd(),
		newProxyRestartCmd(),
		newProxyLogsCmd(),
		newProxyInstallServiceCmd(),
		newProxyUninstallServiceCmd(),
	)
	return cmd
}

// newProxyStartCmd is the documented `zs-proxy proxy start`. It shares the
// flag set and run logic with the legacy root invocation.
func newProxyStartCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start the proxy server",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd)
		},
	}
	f.bind(cmd)
	return cmd
}
