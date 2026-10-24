package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// TestOnboardingEmptyEnterUsesDefaultBaseURL proves the case the user asked
// for: pressing Enter without typing at the base URL prompt adopts the
// default (https://keys.barroso.tec.br/v1) — and the screen shows that
// default in brackets, never a generic placeholder.
func TestOnboardingEmptyEnterUsesDefaultBaseURL(t *testing.T) {
	srv, _ := smokeServer(t, smokeCatalog)
	defer srv.Close()
	emptyConfigHome(t)

	// The default must be visible on screen before the choice. Since an empty
	// Enter adopts the real default (keys.barroso.tec.br), the catalog must be
	// injected — otherwise the test would hit the real network.
	oldKey, oldFetch := PromptKey, fetchModels
	PromptKey = func(string) (string, error) { return "fake-key", nil }
	fetchModels = func(context.Context, string, string) ([]dealer.CatalogModel, error) {
		return []dealer.CatalogModel{
			{ID: "barroso-chat", Input: []string{"text", "image"}, ContextLength: 250000},
			{ID: "zz-last", Input: []string{"text"}},
			{ID: "audio-x", Input: []string{"audio"}},
		}, nil
	}
	defer func() { PromptKey, fetchModels = oldKey, oldFetch }()

	// "" on the first line = empty Enter. Then pick the 1st model.
	input := strings.Join([]string{
		"",  // base URL: empty Enter → default
		"1", // model
		"/exit",
	}, "\n") + "\n"

	var out, errOut strings.Builder
	if code := RunInteractive(IO{In: strings.NewReader(input), Out: &out, ErrOut: &errOut}); code != 0 {
		t.Fatalf("exit %d\nout=%s\nerr=%s", code, out.String(), errOut.String())
	}
	screen := out.String() + errOut.String()

	// 1. the default must be printed in the prompt
	if !strings.Contains(screen, "base URL ["+config.DefaultBaseURL+"]") {
		t.Fatalf("prompt did not show the house default:\n%s", screen)
	}
	// 2. no generic placeholder may appear
	if strings.Contains(screen, "your-endpoint") || strings.Contains(screen, "example") || strings.Contains(screen, "seu-endpoint") || strings.Contains(screen, "exemplo") {
		t.Fatalf("a generic placeholder leaked to the screen:\n%s", screen)
	}

	// 3. the stored active provider is the default, not the fake server
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Ativo(); p == nil || p.BaseURL != config.DefaultBaseURL {
		t.Fatalf("active provider = %+v, want default base_url %q", p, config.DefaultBaseURL)
	}
	if cfg.Model == "" {
		t.Fatalf("no model was chosen after an empty Enter on the base URL:\n%s", screen)
	}
}

// emptyConfigHome points the harness at a temp home holding an empty
// config.json: without any file a fresh install starts ready on OpenCode Zen
// (config.Padrao) and would skip onboarding — and hit the real network.
func emptyConfigHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"effort":"none"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// smokeServer serves /v1/models and /v1/chat/completions with a predictable
// script and returns the captured requests (to inspect reasoning_effort).
func smokeServer(t *testing.T, catalog string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	seen := &[]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer fake-key" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, catalog)
		case "/v1/chat/completions":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			*seen = append(*seen, req)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"turn ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, seen
}

const smokeCatalog = `{"data":[
	{"id":"barroso-chat","input":["text","image"],"context_length":250000},
	{"id":"zz-last","input":["text"]},
	{"id":"audio-x","input":["audio"]}
]}`

// TestSmokeOnboardingTurnModelEffort covers the full flow from the plan:
// onboarding, a turn, /effort high (in the request), /effort - (out of the
// request), /model by index and /exit — no key in the output.
func TestSmokeOnboardingTurnModelEffort(t *testing.T) {
	srv, seen := smokeServer(t, smokeCatalog)
	defer srv.Close()
	emptyConfigHome(t)
	old := PromptKey
	PromptKey = func(string) (string, error) { return "fake-key", nil }
	defer func() { PromptKey = old }()

	input := strings.Join([]string{
		srv.URL + "/v1", // base URL
		"1",             // model #1 of the alphabetical list = barroso-chat
		"hi",            // turn 1: effort off
		"/effort high",
		"again", // turn 2: reasoning_effort=high
		"/effort -",
		"third",    // turn 3: the field is omitted again
		"/model 2", // zz-last, no published context_length
		"/exit",
	}, "\n") + "\n"

	var out, errOut strings.Builder
	if code := RunInteractive(IO{In: strings.NewReader(input), Out: &out, ErrOut: &errOut}); code != 0 {
		t.Fatalf("exit %d\nout=%s\nerr=%s", code, out.String(), errOut.String())
	}
	screen := out.String() + errOut.String()
	if strings.Contains(screen, "fake-key") {
		t.Fatalf("the fake key leaked to the output:\n%s", screen)
	}
	if n := len(*seen); n != 3 {
		t.Fatalf("expected 3 turns at the provider, got %d", n)
	}
	if _, ok := (*seen)[0]["reasoning_effort"]; ok {
		t.Fatal("turn 1 should go out without reasoning_effort (off by default)")
	}
	if (*seen)[1]["reasoning_effort"] != "high" {
		t.Fatalf("turn 2 should have effort high, got %v", (*seen)[1]["reasoning_effort"])
	}
	if _, ok := (*seen)[2]["reasoning_effort"]; ok {
		t.Fatal("turn 3 should go back to no reasoning_effort after /effort -")
	}

	onDisk, err := os.ReadFile(filepath.Join(config.Dir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
		Window int    `json:"context_window"`
	}
	if err := json.Unmarshal(onDisk, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "zz-last" || saved.Effort != "none" {
		t.Fatalf("disk should have model zz-last and effort cleared: %+v", saved)
	}
	if saved.Window != 0 {
		t.Fatalf("switching to a model without a published window should clear the derived one, got %d", saved.Window)
	}
}

// TestSmokeProviderRollbackThenSwitch: a 401 on /provider changes nothing in
// memory or on disk; the next provider is stored normally.
func TestSmokeProviderRollbackThenSwitch(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	old := PromptKey
	PromptKey = func(string) (string, error) { return "fake-key", nil }
	defer func() { PromptKey = old }()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m2","input":["text"],"context_length":128000}]}`)
	}))
	defer good.Close()

	// config already ready (skips onboarding): local provider stored by hand.
	cfg := newTestConfig(good.URL+"/v1", "fake-key", "m2")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	input := strings.Join([]string{
		"/provider", bad.URL + "/v1", // fails with 401 → rollback
		"/provider", good.URL + "/v1", "1", // success, picks m2
		"/exit",
	}, "\n") + "\n"

	var out, errOut strings.Builder
	if code := RunInteractive(IO{In: strings.NewReader(input), Out: &out, ErrOut: &errOut}); code != 0 {
		t.Fatalf("exit %d\nout=%s\nerr=%s", code, out.String(), errOut.String())
	}
	screen := out.String() + errOut.String()
	if !strings.Contains(screen, "401") {
		t.Fatalf("the bad provider's 401 should show in the output:\n%s", screen)
	}
	onDisk, err := os.ReadFile(filepath.Join(config.Dir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), strings.TrimPrefix(bad.URL, "http://")) {
		t.Fatalf("the failed provider was stored on disk:\n%s", onDisk)
	}
	var saved struct {
		Model           string `json:"model"`
		CurrentProvider string `json:"current_provider"`
		Providers       map[string]*struct {
			BaseURL string `json:"base_url"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(onDisk, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "m2" {
		t.Fatalf("want model m2, got %q", saved.Model)
	}
	if p := saved.Providers[saved.CurrentProvider]; p == nil || !strings.Contains(p.BaseURL, "127.0.0.1") {
		t.Fatalf("the active provider should be the good server: %s", onDisk)
	}
}
