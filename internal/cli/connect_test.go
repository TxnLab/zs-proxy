/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/wallet"
)

// TestConnect_PluginFlagRefusedBeforeAnyWrite drives the real command, not the
// validator alone, because the property is positional: --plugin runs a third
// party's installer, so a misuse has to be refused ahead of everything else.
// --url skips the running-proxy check, which means each of these would go on to
// configure a tool if the refusal came any later — the untouched HERMES_HOME and
// config path are the evidence that it did not.
// In another home, connect writes no tool's own config, which belongs to the
// user rather than the home. --url skips the running check, so a regressed
// refusal would go on to write opencode's config in the temp XDG dir.
func TestConnect_OtherHomeWritesNoToolConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		refused bool
	}{
		{"write", nil, true},
		{"--print", []string{"--print"}, false},
		{"--config-path", []string{"--config-path", "CONF"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(wallet.HomeEnv, t.TempDir())
			t.Setenv("HOME", t.TempDir())
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			conf := filepath.Join(t.TempDir(), "opencode.json")
			args := append([]string{"opencode", "--url", "http://127.0.0.1:1/v1", "--yes"}, tc.args...)
			for i, a := range args {
				if a == "CONF" {
					args[i] = conf
				}
			}
			cmd := newConnectCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.Execute()
			refused := err != nil && strings.Contains(err.Error(), wallet.HomeEnv)
			if refused != tc.refused {
				t.Fatalf("err = %v, refused %v, want %v\n%s", err, refused, tc.refused, out.String())
			}
			if entries, _ := os.ReadDir(xdg); tc.refused && len(entries) > 0 {
				t.Errorf("a refused run wrote %v under XDG_CONFIG_HOME", entries)
			}
		})
	}
}

func TestConnect_PluginFlagRefusedBeforeAnyWrite(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"another tool", []string{"opencode", "--plugin"}, "only applies to `zs-proxy connect hermes`"},
		{"no tool named", []string{"--plugin"}, "only applies to `zs-proxy connect hermes`"},
		{"with --all", []string{"--all", "--plugin"}, "only applies to `zs-proxy connect hermes`"},
		{"hermes with --all", []string{"hermes", "--all", "--plugin"}, "only applies to `zs-proxy connect hermes`"},
		{"with --print", []string{"hermes", "--plugin", "--print"}, "--print"},
		{"with --dry-run", []string{"hermes", "--plugin", "--dry-run"}, "--print"},
		{"with --config-path", []string{"hermes", "--plugin", "--config-path", "CONF"}, "--config-path"},
		{"with --list", []string{"hermes", "--plugin", "--list"}, "--list"},
		{"unknown tool reports the name", []string{"hermez", "--plugin"}, `unknown tool "hermez"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HERMES_HOME", home)
			t.Setenv("HOME", t.TempDir())
			// opencode resolves through $XDG_CONFIG_HOME, so without this a
			// regressed refusal would merge a provider into the developer's own
			// opencode.jsonc on the way to failing.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			conf := filepath.Join(t.TempDir(), "config.yaml")
			args := append([]string{"--url", "http://127.0.0.1:1/v1", "--yes"}, tc.args...)
			for i, a := range args {
				if a == "CONF" {
					args[i] = conf
				}
			}

			cmd := newConnectCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute error = %v, want one containing %q", err, tc.wantErr)
			}
			if entries, _ := os.ReadDir(home); len(entries) != 0 {
				t.Errorf("HERMES_HOME written to despite the refusal: %v", entries)
			}
			if _, statErr := os.Stat(conf); !os.IsNotExist(statErr) {
				t.Errorf("--config-path target exists despite the refusal (stat err %v)", statErr)
			}
		})
	}
}

// TestConnect_PluginFlagReachesApply is the accept half, and it is the one that
// proves the feature is wired: every other case here is a refusal, so a build
// where --plugin is dropped from ApplyOptions — or where the validator refuses
// the one valid combination — passes all of them.
//
// It asserts through the real command with no `hermes` on PATH, so the install
// attempt fails in a way only reachable when InstallPlugin arrived set. Without
// the flag the same run must succeed, which is what separates "the flag was
// honored" from "this errors either way".
func TestConnect_PluginFlagReachesApply(t *testing.T) {
	tests := []struct {
		name    string
		plugin  bool
		wantErr string
	}{
		{"flag set reaches the installer", true, "not on PATH"},
		{"flag absent installs nothing", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HERMES_HOME", home)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			// An empty PATH is what makes the install fail loudly instead of
			// running a real Hermes on the developer's machine.
			t.Setenv("PATH", t.TempDir())

			args := []string{"hermes", "--url", "http://127.0.0.1:1/v1", "--yes"}
			if tc.plugin {
				args = append(args, "--plugin")
			}
			cmd := newConnectCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.Execute()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Execute without --plugin = %v, want success", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute with --plugin = %v, want an error containing %q", err, tc.wantErr)
			}
			// Either way the config is written — the install is an extra step,
			// never a precondition.
			for _, name := range []string{"config.yaml", ".env"} {
				if _, statErr := os.Stat(filepath.Join(home, name)); statErr != nil {
					t.Errorf("%s not written (%v)", name, statErr)
				}
			}
		})
	}
}
