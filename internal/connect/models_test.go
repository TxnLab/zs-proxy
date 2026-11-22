/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestFetchModels_PrefersDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/zs/details":
			w.Write([]byte(`{"models":[{"id":"qwen3-coder","context_window":262144,"max_output_tokens":65536,"input_modalities":["text"]},{"id":"glm-4.6","context_window":200000}],"builtin_tools":[]}`))
		default:
			t.Errorf("unexpected path %s (should have used details)", r.URL.Path)
		}
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].ID != "qwen3-coder" || models[0].ContextWindow != 262144 || models[0].MaxOutputTokens != 65536 {
		t.Errorf("model[0] = %+v", models[0])
	}
	if len(models[0].InputModalities) != 1 || models[0].InputModalities[0] != "text" {
		t.Errorf("model[0] modalities = %v", models[0].InputModalities)
	}
}

// TestFetchModels_Capabilities proves the advertised tool_use / reasoning
// capability signals survive the fetch, with nil tool_use ("unadvertised") kept
// distinct from an explicit false.
func TestFetchModels_Capabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[
			{"id":"full","tool_use":true,"reasoning":{"supported":true,"allowed_efforts":["low","high"]}},
			{"id":"no-tools","tool_use":false,"reasoning":{"supported":false}},
			{"id":"silent"}
		],"builtin_tools":[]}`))
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if f := byID["full"]; f.ToolUse == nil || !*f.ToolUse || !f.Reasoning {
		t.Errorf("full = %+v, want tool_use *true and reasoning true", f)
	}
	if n := byID["no-tools"]; n.ToolUse == nil || *n.ToolUse || n.Reasoning {
		t.Errorf("no-tools = %+v, want tool_use *false and reasoning false", n)
	}
	if s := byID["silent"]; s.ToolUse != nil || s.Reasoning {
		t.Errorf("silent = %+v, want tool_use nil (unadvertised) and reasoning false", s)
	}
}

func TestFetchModels_Tags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"id":"tagged","tags":["code","NSFW"]},{"id":"bare"}],"builtin_tools":[]}`))
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if got := byID["tagged"].Tags; !slices.Equal(got, []string{"code", "NSFW"}) {
		t.Errorf("tagged tags = %v, want [code NSFW] verbatim", got)
	}
	if got := byID["bare"].Tags; got != nil {
		t.Errorf("bare tags = %v, want nil", got)
	}
}

func TestWithTags(t *testing.T) {
	models := []Model{
		{ID: "both", Tags: []string{"code", "Uncensored"}},
		{ID: "code-only", Tags: []string{"code"}},
		{ID: "none"},
	}
	ids := func(ms []Model) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		tags []string
		want []string
	}{
		{"no filter keeps all", nil, []string{"both", "code-only", "none"}},
		{"one tag", []string{"code"}, []string{"both", "code-only"}},
		{"case-insensitive", []string{"CODE"}, []string{"both", "code-only"}},
		{"every tag must match", []string{"code", "uncensored"}, []string{"both"}},
		{"no match", []string{"medical"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(WithTags(models, tc.tags)); !slices.Equal(got, tc.want) {
				t.Errorf("WithTags(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestFetchModels_GrossesUpRates(t *testing.T) {
	// The proxy aggregate uses input_usd_per_1m / output_usd_per_1m /
	// cache_read_usd_per_1m and a top-level fee_bps. FetchModels grosses the
	// operator rates up to payer-net (× (1 + feeBps/10000)) and rounds to 6dp.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/zs/details":
			w.Write([]byte(`{"fee_bps":200,"models":[
				{"id":"priced","input_usd_per_1m":0.15,"output_usd_per_1m":0.60,"cache_read_usd_per_1m":0.015},
				{"id":"free-cache","input_usd_per_1m":1.0,"output_usd_per_1m":2.0,"cache_read_usd_per_1m":0},
				{"id":"unpriced"}
			],"builtin_tools":[]}`))
		default:
			t.Errorf("unexpected path %s (should have used details)", r.URL.Path)
		}
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}

	// priced: every rate grossed by 1.02, rounded to 6dp.
	if p := byID["priced"]; p.InputUSDPer1M != 0.153 || p.OutputUSDPer1M != 0.612 {
		t.Errorf("priced rates = in %v / out %v, want 0.153 / 0.612", p.InputUSDPer1M, p.OutputUSDPer1M)
	} else if p.CacheReadUSDPer1M == nil || *p.CacheReadUSDPer1M != 0.0153 {
		t.Errorf("priced cache_read = %v, want ptr(0.0153)", p.CacheReadUSDPer1M)
	}

	// free-cache: a present cache_read of 0 stays a pointed-0 (free cached reads),
	// distinct from nil (no discount).
	if fc := byID["free-cache"]; fc.CacheReadUSDPer1M == nil || *fc.CacheReadUSDPer1M != 0 {
		t.Errorf("free-cache cache_read = %v, want ptr(0)", fc.CacheReadUSDPer1M)
	}

	// unpriced: no rates advertised → zero/nil (integrations write no cost block).
	if u := byID["unpriced"]; u.InputUSDPer1M != 0 || u.OutputUSDPer1M != 0 || u.CacheReadUSDPer1M != nil {
		t.Errorf("unpriced carries rates: %+v", u)
	}
}

func TestFetchModels_FallsBackToModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/zs/details":
			w.Write([]byte(`{"models":[],"builtin_tools":[]}`)) // empty → fall back
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[{"id":"a"},{"id":"b"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Fatalf("got %+v, want [a b]", models)
	}
}

func TestDefaultModel(t *testing.T) {
	cases := []struct {
		name   string
		models []Model
		want   string
	}{
		{"empty", nil, ""},
		{"prefers coder", []Model{{ID: "glm-4.6"}, {ID: "qwen3-coder"}}, "qwen3-coder"},
		{"first when none coding", []Model{{ID: "glm-4.6"}, {ID: "gemini"}}, "glm-4.6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultModel(tc.models); got != tc.want {
				t.Errorf("DefaultModel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChatModels(t *testing.T) {
	in := []Model{
		{ID: "no-signal"}, // empty → text-only → keep
		{ID: "text-text", InputModalities: []string{"text"}, OutputModalities: []string{"text"}},       // keep
		{ID: "vision", InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}}, // keep (text out)
		{ID: "img-gen", InputModalities: []string{"text"}, OutputModalities: []string{"image"}},        // drop (no text out)
		{ID: "img-only-in", InputModalities: []string{"image"}, OutputModalities: []string{"text"}},    // drop (no text in)
		{ID: "img-in-default-out", InputModalities: []string{"image"}},                                 // drop (no text in; out defaults text)
	}
	got := ChatModels(in)
	want := []string{"no-signal", "text-text", "vision"}
	if len(got) != len(want) {
		t.Fatalf("got %d models %v, want %v", len(got), ids(got), want)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("got[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
}

func ids(models []Model) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.ID
	}
	return out
}

func TestHasModel(t *testing.T) {
	models := []Model{{ID: "a"}, {ID: "b"}}
	if !HasModel(models, "b") {
		t.Error("HasModel(b) = false")
	}
	if HasModel(models, "z") {
		t.Error("HasModel(z) = true")
	}
	// "" is the no-model-selected signal, never a lookup that can succeed.
	if HasModel([]Model{{ID: ""}}, "") {
		t.Error(`HasModel("") = true`)
	}
}

// TestSortModels covers the ordering `connect` shows in its picker (and writes
// into every generated catalog): by id, case-insensitively, so an org-prefixed
// id sorts where a reader expects rather than ahead of everything lowercase.
func TestSortModels(t *testing.T) {
	models := []Model{
		{ID: "zephyr"},
		{ID: "GLM-4.6"},
		{ID: "alpha"},
		{ID: "glm-4.5"},
		{ID: "Alpha"},
		{ID: "qwen3-coder"},
	}
	SortModels(models)
	// GLM-4.6 sits between the two glm-4.x entries rather than being hoisted
	// ahead of everything lowercase, and the case-only pair has a fixed order
	// instead of one that depends on how the registry happened to emit them.
	want := []string{"Alpha", "alpha", "glm-4.5", "GLM-4.6", "qwen3-coder", "zephyr"}
	if got := ids(models); !equalStrings(got, want) {
		t.Errorf("SortModels = %v, want %v", got, want)
	}
	// Empty and nil are the no-model-advertised paths — the CLI sorts before it
	// knows whether anything came back.
	SortModels(nil)
	SortModels([]Model{})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
