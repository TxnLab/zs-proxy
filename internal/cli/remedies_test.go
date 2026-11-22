/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/app"
)

// The remedies and the doctor's hints are the only place a renamed subcommand
// leaves stale advice behind: they are prose, so nothing else would fail.
func TestRemediesNameRegisteredCommands(t *testing.T) {
	root := newRootCmd()
	seen := 0
	for name, remedies := range map[string]any{"Remedies": app.Remedies, "SolanaRemedies": app.SolanaRemedies} {
		r := reflect.ValueOf(remedies)
		for i := range r.NumField() {
			seen += checkNamedCommands(t, root, name+"."+r.Type().Field(i).Name, r.Field(i).String())
		}
	}
	if seen < 8 {
		t.Fatalf("found %d commands in the remedies; the pattern is looking for the wrong thing", seen)
	}
	for name, hints := range map[string]map[string]map[zs.DoctorStatus]string{"doctorHints": doctorHints, "solanaDoctorHints": solanaDoctorHints} {
		for id, byStatus := range hints {
			for status, hint := range byStatus {
				where := fmt.Sprintf("%s[%s][%s]", name, id, status)
				if checkNamedCommands(t, root, where, hint) == 0 {
					t.Errorf("%s names no `zs-proxy` command: %q", where, hint)
				}
			}
		}
	}
}

var quotedCommand = regexp.MustCompile("`zs-proxy ([^`]+)`")

// checkNamedCommands reports each `zs-proxy …` in text that names no
// registered subcommand, and returns how many it found.
func checkNamedCommands(t *testing.T, root *cobra.Command, where, text string) int {
	t.Helper()
	ms := quotedCommand.FindAllStringSubmatch(text, -1)
	for _, m := range ms {
		cmd := root
		for _, word := range strings.Fields(m[1]) {
			if strings.HasPrefix(word, "<") {
				continue // a placeholder argument
			}
			var next = cmd
			for _, sub := range cmd.Commands() {
				if sub.Name() == word {
					next = sub
					break
				}
			}
			if next == cmd {
				t.Errorf("%s names `zs-proxy %s`: %q has no subcommand %q", where, m[1], cmd.CommandPath(), word)
				break
			}
			cmd = next
		}
	}
	return len(ms)
}
