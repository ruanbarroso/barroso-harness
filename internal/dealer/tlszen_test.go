package dealer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEhHostZen(t *testing.T) {
	for host, want := range map[string]bool{
		"opencode.ai": true, "OpenCode.ai.": true, "api.opencode.ai": true,
		"notopencode.ai": false, "example.com": false, "": false,
	} {
		if got := ehHostZen(host); got != want {
			t.Errorf("ehHostZen(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSpecBunAplica(t *testing.T) {
	s := specBun()
	if len(s.CipherSuites) != 17 || s.CipherSuites[0] != 0x1301 || s.CipherSuites[16] != 0x0035 {
		t.Fatalf("cipher list is not Bun's: %x", s.CipherSuites)
	}
	if len(s.Extensions) != 13 {
		t.Fatalf("extensions = %d, want 13 (Bun's hello)", len(s.Extensions))
	}
}

// Non-zen hosts keep the stdlib transport (plain http test server works).
func TestRoteadorTLSOutrosHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	resp, err := clienteHTTP.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestComBlocoEnv(t *testing.T) {
	out := comBlocoEnv("You are Bruce.")
	if !strings.HasPrefix(out, "You are Bruce.\n\n") || !strings.Contains(out, "<env>\n  Working directory: ") || !strings.HasSuffix(out, "</env>") {
		t.Fatalf("env block missing or malformed: %q", out)
	}
	ja := "x\n<env>\n  Working directory: /a\n</env>"
	if comBlocoEnv(ja) != ja {
		t.Fatal("a system that already has the env block must stay untouched")
	}
	if !strings.HasPrefix(comBlocoEnv(""), "Here is some useful information") {
		t.Fatal("empty system must become the env block")
	}
}
