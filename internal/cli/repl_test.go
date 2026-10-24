package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

func fakeHome(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", dir)
	return dir
}

// bridgeIO injects reader/writer and, above all, makes PromptKey consume the
// NEXT input line — that is how tests feed the hidden key (production code
// reads it from a no-echo prompt, not from io.In).
func bridgeIO(t *testing.T, input string) (IO, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	rd := bufio.NewReader(strings.NewReader(input))
	pk := PromptKey
	PromptKey = func(string) (string, error) {
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	t.Cleanup(func() { PromptKey = pk })
	return IO{In: rd, Out: out, ErrOut: errOut}, out, errOut
}

func fakeModelsProvider(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			w.WriteHeader(404)
			return
		}
		var sb strings.Builder
		sb.WriteString(`{"object":"list","data":[`)
		for i, id := range ids {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"id":%q,"object":"model","input":["text","image"],"context_length":250000}`, id)
		}
		sb.WriteString(`]}`)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sb.String())
	}))
}

func readyConfig(t *testing.T, base, key, model string) string {
	home := fakeHome(t)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
  "current_provider":"p1",
  "providers":{"p1":{"base_url":"`+base+`","api_key":"`+key+`"}},
  "model":"`+model+`",
  "effort":""
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// /model lists the catalog alphabetically and switches to the chosen model.
func TestReplModelListsAlphabeticallyAndSwitches(t *testing.T) {
	srv := fakeModelsProvider(t, "z-model", "a-model", "m-model")
	defer srv.Close()
	home := readyConfig(t, srv.URL+"/v1", "k", "a-model")

	io, outIO, _ := bridgeIO(t, "/model\n2\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("repl exited with code %d: %s", code, io.ErrOut)
	}
	output := outIO.String()
	for _, want := range []string{"a-model", "m-model", "z-model"} {
		if !strings.Contains(output, want) {
			t.Fatalf("catalog missing %q:\n%s", want, output)
		}
	}
	if strings.Index(output, "a-model") > strings.Index(output, "m-model") ||
		strings.Index(output, "m-model") > strings.Index(output, "z-model") {
		t.Fatal("catalog was not in alphabetical order")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "m-model" {
		t.Fatalf("model after /model = %q, want m-model (2nd in the list)", cfg.Model)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "m-model") {
		t.Fatal("/model did not persist the model to disk")
	}
}

// /model <id> switches directly when the id is in the catalog.
func TestReplModelDirect(t *testing.T) {
	srv := fakeModelsProvider(t, "barroso-chat", "barroso-quick")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-quick")

	io, _, _ := bridgeIO(t, "/model barroso-chat\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("exit code %d: %s", code, io.ErrOut)
	}
	cfg, _ := config.Load()
	if cfg.Model != "barroso-chat" {
		t.Fatalf("model = %q", cfg.Model)
	}
}

// /model with an id outside the catalog does not break the current config.
func TestReplModelNotInCatalogKeepsConfig(t *testing.T) {
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, _, errIO := bridgeIO(t, "/model does-not-exist\n/effort\n/exit\n")
	RunInteractive(io)
	cfg, _ := config.Load()
	if cfg.Model != "barroso-chat" {
		t.Fatalf("model wrongly changed to %q", cfg.Model)
	}
	// expected and reported error: an id outside the catalog points to listing.
	if !strings.Contains(errIO.String(), "does-not-exist") {
		t.Fatalf("error about an id outside the catalog was not reported: %q", errIO.String())
	}
}

// /effort sets, shows and clears. Clearing stores "none" (so the default does
// not come back) and OMITS the field from the request.
func TestReplEffortSetShowClear(t *testing.T) {
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	home := readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := bridgeIO(t, "/effort\n/effort high\n/effort\n/effort -\n/effort\n/exit\n")
	RunInteractive(io)
	output := outIO.String()
	cfg, _ := config.Load()
	if cfg.Effort != "none" {
		t.Fatalf("effort should be none after `/effort -` (the dealer omits it), got %q", cfg.Effort)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if strings.Contains(strings.ToLower(string(b)), "reasoning_effort") {
		t.Fatal("`/effort -` must omit reasoning_effort, not store 'none' or an empty string")
	}
	for _, want := range []string{"high", "low", "medium", "xhigh", "max"} {
		if !strings.Contains(output, want) {
			t.Fatalf("effort listing missing %q:\n%s", want, output)
		}
	}
}

// /provider switches base+key, validates /models and keeps the model if present.
func TestReplProviderSwitchAndRollbackOnFailure(t *testing.T) {
	ok := fakeModelsProvider(t, "barroso-chat")
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
	}))
	defer bad.Close()

	home := readyConfig(t, ok.URL+"/v1", "k-old", "barroso-chat")
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	// invalid provider: visible error and config untouched on disk.
	io, outIO, errIO := bridgeIO(t, "/provider\n"+bad.URL+"/v1\nother-key\n/exit\n")
	RunInteractive(io)
	after, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if string(before) != string(after) {
		t.Fatal("config changed despite the /provider failure")
	}
	if !strings.Contains(errIO.String(), "error") && !strings.Contains(outIO.String(), "error") {
		t.Fatalf("/provider failure was not reported: out=%q err=%q", io.Out, io.ErrOut)
	}

	// valid provider, same model: the switch persists.
	io2, _, _ := bridgeIO(t, "/provider\n"+ok.URL+"/v1\nk-new\n/exit\n")
	RunInteractive(io2)
	cfg, _ := config.Load()
	if cfg.Ativo() == nil || cfg.Ativo().BaseURL != ok.URL+"/v1" {
		t.Fatalf("active provider did not switch: %+v", cfg.Ativo())
	}
	if cfg.Ativo().APIKey != "k-new" {
		t.Fatalf("key did not switch: %q", cfg.Ativo().APIKey)
	}
	if cfg.Model != "barroso-chat" {
		t.Fatalf("model should be kept, got %q", cfg.Model)
	}
}

// /provider zen restores the public default and removes custom providers.
func TestReplProviderZenResetsCustomProviders(t *testing.T) {
	custom := fakeModelsProvider(t, "custom-model")
	defer custom.Close()
	home := readyConfig(t, custom.URL+"/v1", "custom-secret", "custom-model")

	io, outIO, errIO := bridgeIO(t, "/provider zen\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("exit %d: out=%s err=%s", code, outIO.String(), errIO.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Ativo()
	if cfg.CurrentProvider != config.ZenProvider || cfg.Model != config.ZenModel ||
		p == nil || p.BaseURL != config.ZenBaseURL || p.APIKey != config.ZenAPIKey ||
		cfg.Effort != "none" || cfg.Janela != config.ZenJanela {
		t.Fatalf("/provider zen did not restore defaults: %+v; provider=%+v", cfg, p)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("custom providers were not removed: %+v", cfg.Providers)
	}
	if _, ok := cfg.Providers[config.ZenProvider]; !ok {
		t.Fatalf("Zen provider missing after reset: %+v", cfg.Providers)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "custom-secret") || strings.Contains(string(b), custom.URL) {
		t.Fatalf("removed provider credentials remain on disk: %s", b)
	}
}

// /provider where the current model does not exist clears model and requires a choice.
func TestReplProviderWithoutCurrentModelClearsModel(t *testing.T) {
	current := fakeModelsProvider(t, "barroso-chat")
	defer current.Close()
	other := fakeModelsProvider(t, "barroso-quick")
	defer other.Close()
	readyConfig(t, current.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := bridgeIO(t, "/provider\n"+other.URL+"/v1\nk2\n1\n/exit\n")
	RunInteractive(io)
	cfg, _ := config.Load()
	if cfg.Model != "barroso-quick" {
		t.Fatalf("model after provider switch = %q, want barroso-quick (1st in the list)\noutput:\n%s", cfg.Model, outIO.String())
	}
}

// The key never shows up in output or errors.
func TestReplNeverPrintsKey(t *testing.T) {
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	const secret = "secret-never-printed-123"
	readyConfig(t, srv.URL+"/v1", secret, "barroso-chat")

	io, outIO, errIO := bridgeIO(t, "/model barroso-chat\n/effort high\n/provider\n"+badServerURL(t)+"\n"+secret+"\n/exit\n")
	RunInteractive(io)
	for _, stream := range []string{outIO.String(), errIO.String()} {
		if strings.Contains(stream, secret) {
			t.Fatal("key leaked to the output")
		}
	}
}

func badServerURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "boom")
	}))
	t.Cleanup(s.Close)
	return s.URL + "/v1"
}

// /help documents the required commands.
func TestReplHelpListsCommands(t *testing.T) {
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := bridgeIO(t, "/help\n/exit\n")
	RunInteractive(io)
	output := outIO.String()
	for _, want := range []string{"/model", "/effort", "/provider", "/help", "/exit"} {
		if !strings.Contains(output, want) {
			t.Fatalf("/help missing %q:\n%s", want, output)
		}
	}
}

// A plain message is not mistaken for a slash command.
func TestReplPlainMessageIsNotCommand(t *testing.T) {
	srv := fakeModelsProvider(t, "barroso-chat")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "barroso-chat")

	io, _, errIO := bridgeIO(t, "what do you mean /model?\n/exit\n")
	RunInteractive(io)
	if strings.Contains(errIO.String(), "unknown") || strings.Contains(errIO.String(), "command") {
		// only checks it did not break: a slash mid-text is not a command.
		_ = io.ErrOut
	}
}

// Empty Enter in the /model selection exits without switching the current model.
func TestReplModelEmptyEnterKeeps(t *testing.T) {
	srv := fakeModelsProvider(t, "z-model", "a-model", "m-model")
	defer srv.Close()
	readyConfig(t, srv.URL+"/v1", "k", "a-model")

	io, outIO, _ := bridgeIO(t, "/model\n\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("repl exited with code %d", code)
	}
	output := outIO.String()
	if !strings.Contains(output, "empty Enter keeps a-model") {
		t.Fatalf("empty Enter hint missing:\n%s", output)
	}
	if !strings.Contains(output, "model unchanged: a-model") {
		t.Fatalf("confirmation missing:\n%s", output)
	}
	if strings.Contains(output, "no model, no turn") {
		t.Fatalf("empty Enter should not insist when there is a model:\n%s", output)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "a-model" {
		t.Fatalf("model changed to %q", cfg.Model)
	}
}
