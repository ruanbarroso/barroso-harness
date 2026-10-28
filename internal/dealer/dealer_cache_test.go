package dealer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The provider's prompt_tokens_details.cached_tokens reaches Usage, and the
// round records its latency.
func TestRoundReadsCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":2823,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":2816}}}`,
			`data: [DONE]`, "",
		}, "\n\n")))
	}))
	defer srv.Close()
	r, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil || r.Usage.PromptTokens != 2823 || r.Usage.CachedTokens != 2816 {
		t.Fatalf("usage %+v", r.Usage)
	}
	if r.TTFT <= 0 || r.Dur < r.TTFT {
		t.Fatalf("ttft %v dur %v", r.TTFT, r.Dur)
	}
}
