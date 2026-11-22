/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestServiceSpec_AbsolutizesConfig asserts a relative --config is made absolute
// (a service runs from a different cwd, so a relative path would not resolve)
// and that the args carry the foreground invocation.
func TestServiceSpec_AbsolutizesConfig(t *testing.T) {
	f := proxyFlags{configPath: "cfg/config.yaml", network: "testnet"}
	spec, err := f.serviceSpec()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.HasPrefix(joined, "proxy start --foreground") {
		t.Errorf("args don't start with the foreground invocation: %q", joined)
	}
	// The --config value must be an absolute path.
	var cfgVal string
	for i, a := range spec.Args {
		if a == "--config" && i+1 < len(spec.Args) {
			cfgVal = spec.Args[i+1]
		}
	}
	if cfgVal == "" {
		t.Fatal("--config not present in service args")
	}
	if !filepath.IsAbs(cfgVal) {
		t.Errorf("--config %q is not absolute", cfgVal)
	}
	if !strings.HasSuffix(cfgVal, filepath.Join("cfg", "config.yaml")) {
		t.Errorf("--config %q lost the original relative tail", cfgVal)
	}
	if spec.BinPath == "" {
		t.Error("BinPath should be the resolved executable")
	}
}

func TestServiceSpec_NoConfigWhenUnset(t *testing.T) {
	f := proxyFlags{configPath: "", network: "mainnet"}
	spec, err := f.serviceSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range spec.Args {
		if a == "--config" {
			t.Errorf("unset config should not produce a --config arg: %v", spec.Args)
		}
	}
}

func TestProxyServiceCommandsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, sub := range []string{"install-service", "uninstall-service"} {
		if _, _, err := root.Find([]string{"proxy", sub}); err != nil {
			t.Errorf("proxy %s not found: %v", sub, err)
		}
	}
	// install-service exposes --network/--config/--port/--print, plus the Windows
	// --service-account and the internal (hidden) --uac-relaunch.
	ic, _, err := root.Find([]string{"proxy", "install-service"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fl := range []string{"network", "config", "port", "print", "service-account", "uac-relaunch"} {
		if ic.Flags().Lookup(fl) == nil {
			t.Errorf("install-service missing --%s", fl)
		}
	}
	// --uac-relaunch is an internal relaunch marker — keep it hidden from help.
	if f := ic.Flags().Lookup("uac-relaunch"); f != nil && !f.Hidden {
		t.Error("--uac-relaunch should be hidden")
	}
}
