/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const modelsTestCatalog = `{"fee_bps":0,"models":[
	{"id":"coder","context_window":131072,"input_usd_per_1m":0.5,"output_usd_per_1m":1.5,"tool_use":true,"tags":["code"]},
	{"id":"wild","tags":["Uncensored","code"],"input_modalities":["text","image"]},
	{"id":"plain"}
],"builtin_tools":[]}`

func modelsTestProxy(t *testing.T, catalog string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/zs/details" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(catalog))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func execModels(t *testing.T, catalog string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newModelsCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--url", modelsTestProxy(t, catalog)}, args...))
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func runModels(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	out, errOut, err := execModels(t, modelsTestCatalog, args...)
	if err != nil {
		t.Fatalf("Execute: %v (stderr %q)", err, errOut)
	}
	return out, errOut
}

// tableRows splits the table into rows of whitespace-separated cells. No cell
// in the fixtures contains a space, so the split is exact.
func tableRows(out string) map[string][]string {
	rows := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		cells := strings.Fields(line)
		rows[cells[0]] = cells
	}
	return rows
}

func TestModels_TableShowsTags(t *testing.T) {
	out, _ := runModels(t)
	if !strings.HasPrefix(out, "MODEL") {
		t.Fatalf("no header:\n%s", out)
	}
	rows := tableRows(out)
	want := map[string][]string{
		"coder": {"coder", "128K", "0.50", "1.50", "tools", "code"},
		"wild":  {"wild", "-", "-", "-", "vision", "Uncensored,code"},
		"plain": {"plain", "-", "-", "-", "-", "-"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%s", len(rows), len(want), out)
	}
	for id, cells := range want {
		if got := strings.Join(rows[id], " "); got != strings.Join(cells, " ") {
			t.Errorf("%s row = %q, want %q", id, got, strings.Join(cells, " "))
		}
	}
}

// A node writes the tags, and the table goes to a terminal.
func TestModels_TableNeutralizesControlCharacters(t *testing.T) {
	out, _, err := execModels(t, `{"models":[{"id":"m","tags":["a\u001b[2Jb","c\td"]}]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.ContainsAny(out, "\x1b\t") {
		t.Errorf("control characters reached the table: %q", out)
	}
	if got := tableRows(out)["m"]; len(got) == 0 || got[len(got)-1] != "a?[2Jb,c?d" {
		t.Errorf("m row = %q, want tags rendered as a?[2Jb,c?d", got)
	}
}

func TestModels_TagFilter(t *testing.T) {
	ids := func(out string) []string {
		var got []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		var s []string
		for _, m := range got {
			s = append(s, m.ID)
		}
		return s
	}
	out, _ := runModels(t, "--json", "--tag", "CODE")
	if got := strings.Join(ids(out), ","); got != "coder,wild" {
		t.Errorf("--tag CODE = %s, want coder,wild", got)
	}
	out, _ = runModels(t, "--json", "--tag", "code", "--tag", "uncensored")
	if got := strings.Join(ids(out), ","); got != "wild" {
		t.Errorf("--tag code --tag uncensored = %s, want wild", got)
	}
}

func TestModels_JSONTagsNeverNull(t *testing.T) {
	out, _ := runModels(t, "--json")
	if strings.Contains(out, `"tags": null`) {
		t.Errorf("untagged model rendered tags as null:\n%s", out)
	}
	if !strings.Contains(out, `"tags": []`) {
		t.Errorf("untagged model missing an empty tags array:\n%s", out)
	}
}

func TestModels_NoMatchSaysSo(t *testing.T) {
	out, errOut := runModels(t, "--tag", "medical")
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "No models tagged medical") {
		t.Errorf("stderr = %q, want a no-match notice", errOut)
	}
}

func TestModels_NoMatchJSONIsEmptyArray(t *testing.T) {
	out, _ := runModels(t, "--json", "--tag", "medical")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("stdout = %q, want []", out)
	}
}

// A catalog with no tags anywhere is what an older proxy serves, so the notice
// must not claim the filter found nothing to match.
func TestModels_UntaggedCatalogPointsAtTheProxy(t *testing.T) {
	_, errOut, err := execModels(t, `{"models":[{"id":"a"},{"id":"b"}]}`, "--tag", "code")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(errOut, "No model in the catalog carries any tags") || !strings.Contains(errOut, "restart") {
		t.Errorf("stderr = %q, want the untagged-catalog notice", errOut)
	}
	if strings.Contains(errOut, "No models tagged") {
		t.Errorf("stderr = %q, gave the plain no-match notice for an untagged catalog", errOut)
	}
}

func TestModels_TagValueIsTrimmedAndRequired(t *testing.T) {
	out, _ := runModels(t, "--json", "--tag", " code ")
	if !strings.Contains(out, `"coder"`) {
		t.Errorf("--tag ' code ' missed coder:\n%s", out)
	}
	if _, _, err := execModels(t, modelsTestCatalog, "--tag", " "); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("--tag ' ' error = %v, want a non-empty refusal", err)
	}
}
