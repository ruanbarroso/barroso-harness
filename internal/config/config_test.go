// Package config — tests: default URL, legacy shape migration, canonical
// Save/Load (providers/current_provider), empty effort and the secret kept
// out of error/status output.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func home(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", dir)
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvAuthToken, "")
	return dir, func() {}
}

// First install without BARROSO_* env: ready on OpenCode Zen + big-pickle.
func TestLoadWithoutFileReturnsZenDefault(t *testing.T) {
	home(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("missing config.json must not be an error: %v", err)
	}
	p := c.Ativo()
	if !c.Pronta() || c.FromEnv() || c.CurrentProvider != ZenProvider || c.Model != ZenModel ||
		p.BaseURL != ZenBaseURL || p.APIKey != ZenAPIKey || c.Janela != ZenJanela {
		t.Fatalf("want the Zen default, got %+v (provider %+v)", c, p)
	}
	if strings.Contains(c.Resumo(), ZenAPIKey+" ") {
		t.Fatalf("status line leaked the key: %s", c.Resumo())
	}
}

func TestLoadEmptyFileIsNotReady(t *testing.T) {
	dir, _ := home(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(); c.Pronta() {
		t.Fatal("an existing empty config is not ready (the default is only for first install)")
	}
}

func TestInitWritesZenDefaultWithoutEnv(t *testing.T) {
	home(t)
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pronta() || c.CurrentProvider != ZenProvider || c.Model != ZenModel || c.Effort != "none" {
		t.Fatalf("Init did not persist the Zen default: %+v", c)
	}
}

func TestInitWithEnvKeepsEmptySkeleton(t *testing.T) {
	dir, _ := home(t)
	t.Setenv(EnvAuthToken, "env-token")
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(b), ZenBaseURL) || strings.Contains(string(b), "env-token") {
		t.Fatalf("with env set, Init must not write the Zen default nor the env token:\n%s", b)
	}
}

func TestSaveLoadRoundTripWithProvider(t *testing.T) {
	home(t)
	c := Config{Model: "barroso-chat", Effort: ""}
	c = setTestProvider(c, "keys-barroso", DefaultBaseURL, "secret-key")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := got.Ativo()
	if p == nil || p.BaseURL != DefaultBaseURL || p.APIKey != "secret-key" {
		t.Fatalf("wrong active provider: %+v", p)
	}
	// No effort saved: Load fills in the default level.
	if got.Model != "barroso-chat" || got.Effort != DefaultEffort {
		t.Fatalf("wrong model/effort: %+v", got)
	}
}

func setTestProvider(c Config, name, base, key string) Config {
	if c.Providers == nil {
		c.Providers = map[string]*Provider{}
	}
	c.Providers[name] = &Provider{BaseURL: base, APIKey: key}
	c.CurrentProvider = name
	c.BaseURL, c.APIKey = base, key
	return c
}

func TestDiskJSONHasCanonicalShape(t *testing.T) {
	home(t)
	c := setTestProvider(Config{Model: "m"}, "p", "https://x/v1", "k123")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(Dir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		CurrentProvider string               `json:"current_provider"`
		Providers       map[string]*Provider `json:"providers"`
		BaseURLRoot     string               `json:"base_url"`
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.BaseURLRoot != "" {
		t.Fatalf("base_url must not be duplicated at the root: %q", disk.BaseURLRoot)
	}
	if disk.CurrentProvider != "p" || disk.Providers["p"].BaseURL != "https://x/v1" {
		t.Fatalf("canonical shape violated: %s", b)
	}
}

func TestLegacyShapeMigration(t *testing.T) {
	home(t)
	legacy := []byte(`{"base_url":"https://keys.barroso.tec.br/v1","model":"barroso-chat"}`)
	if err := os.WriteFile(filepath.Join(Dir(), "config.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentProvider != "keys.barroso" {
		t.Fatalf("wrong migrated provider: %q", c.CurrentProvider)
	}
	if c.Ativo().BaseURL != DefaultBaseURL {
		t.Fatalf("wrong migrated base: %q", c.Ativo().BaseURL)
	}
	if c.Model != "barroso-chat" {
		t.Fatalf("migrated model lost: %q", c.Model)
	}
	if c.Pronta() {
		t.Fatal("without a key on disk it is not ready (asked interactively)")
	}
	// persisting the migration keeps the new shape
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, _ := Load()
	if got.CurrentProvider != "keys.barroso" || got.Model != "barroso-chat" {
		t.Fatalf("migration round trip failed: %+v", got)
	}
}

func TestMigrationWithExternalURL(t *testing.T) {
	home(t)
	legacy := []byte(`{"base_url":"https://other.example/v1","model":"m","context_window":16000}`)
	if err := os.WriteFile(filepath.Join(Dir(), "config.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := Load()
	if c.CurrentProvider != "default" {
		t.Fatalf("expected the default provider, got %q", c.CurrentProvider)
	}
	if c.Janela != 16000 || c.JanelaSource != FonteManual {
		t.Fatalf("manual window not preserved: %d/%s", c.Janela, c.JanelaSource)
	}
}

func TestSummaryNeverContainsKey(t *testing.T) {
	c := setTestProvider(Config{Model: "m", Effort: "high"}, "p", "https://x/v1", "SECRET-123")
	s := c.Resumo()
	if strings.Contains(s, "SECRET") {
		t.Fatalf("Resumo leaked the key: %s", s)
	}
	f := strings.Join(c.Faltas(), "; ")
	if strings.Contains(f, "SECRET") {
		t.Fatalf("Faltas leaked the key: %s", f)
	}
}

func TestEmptyEffortOmitsField(t *testing.T) {
	home(t)
	c := setTestProvider(Config{Model: "m"}, "p", "https://x/v1", "k")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(), "config.json"))
	if strings.Contains(string(b), `"effort"`) {
		t.Fatalf("an empty effort must not appear in the JSON: %s", b)
	}
}

func TestSelectProviderKeepsManualWindow(t *testing.T) {
	c := Config{Janela: 32000, JanelaSource: FonteManual}
	c.SelecionarProvider("p", 250000)
	if c.Janela != 32000 {
		t.Fatalf("manual window was overwritten: %d", c.Janela)
	}
	c2 := Config{Janela: 1000, JanelaSource: FonteCatalogo}
	c2.SelecionarProvider("p", 250000)
	if c2.Janela != 250000 {
		t.Fatalf("catalog window was not updated: %d", c2.Janela)
	}
	c3 := Config{JanelaSource: FonteCatalogo}
	c3.SelecionarProvider("p", 0)
	if c3.Janela != 0 || c3.JanelaSource != "" {
		t.Fatalf("derived window should reset: %+v", c3)
	}
}
