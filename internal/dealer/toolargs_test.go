package dealer

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func streamResp(lines ...string) *http.Response {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(b.String()))}
}

func TestReadRoundArgsFragments(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"shell","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"echo"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" um\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 1 || r.Tools[0].Args != `{"command": "echo um"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// Upstreams that send the arguments as an object, or calls without an index,
// must not lose the arguments.
func TestReadRoundArgsObjectAndMissingIndex(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":{"path":"x"}}},{"id":"b","type":"function","function":{"name":"read","arguments":{"path":"y"}}}]}}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 || r.Tools[0].Args != `{"path":"x"}` || r.Tools[1].Args != `{"path":"y"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// Two calls sharing index 0 with different ids are two calls, not one.
func TestReadRoundSameIndexNewID(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"read","arguments":"{\"path\":\"y\"}"}}]}}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 || r.Tools[0].ID != "a" || r.Tools[1].Args != `{"path":"y"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// Indexless, idless continuations belong to the call that was last opened,
// across chunks, not to position 0.
func TestReadRoundIndexlessContinuationsAcrossChunks(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"b","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"y\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 ||
		r.Tools[0].ID != "a" || r.Tools[0].Args != `{"path":"x"}` ||
		r.Tools[1].ID != "b" || r.Tools[1].Args != `{"path":"y"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// A new id on an index already taken opens a new call, and the index-only
// continuations that follow go to that new call, not back to the old slot.
func TestReadRoundSameIndexNewIDSeparateContinuation(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","type":"function","function":{"name":"shell","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 ||
		r.Tools[0].ID != "a" || r.Tools[0].Nome != "read" || r.Tools[0].Args != `{"path":"x"}` ||
		r.Tools[1].ID != "b" || r.Tools[1].Nome != "shell" || r.Tools[1].Args != `{"command":"ls"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// An upstream that repeats the whole arguments object in later deltas must
// not glue two JSON documents together: the last object wins.
func TestReadRoundRepeatedJSONObject(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":{"path":"x"}}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"arguments":{"path":"x"}}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":{"path":"x","limit":5}}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 1 || r.Tools[0].Args != `{"path":"x","limit":5}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// parallel_tool_calls=false goes out only when the config asks for it and
// the request carries tools. Corpo always sends the official read/shell
// fingerprint entries, so even an empty tool list goes out with tools.
func TestBodyDisableParallelToolCalls(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "grep", "parameters": map[string]any{"type": "object"}}}}
	for _, tc := range []struct {
		name    string
		disable bool
		tools   []map[string]any
	}{
		{"off/tools", false, tools},
		{"off/nil", false, nil},
		{"on/tools", true, tools},
		{"on/nil", true, nil},
		{"on/empty", true, []map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := requestBody(t, Config{BaseURL: "https://x/v1", Model: "m", DisableParallelToolCalls: tc.disable}, tc.tools)
			wire, _ := m["tools"].([]any)
			v, ok := m["parallel_tool_calls"]
			want := tc.disable && len(wire) > 0
			if ok != want {
				t.Fatalf("parallel_tool_calls present=%v, want %v (wire tools %d)", ok, want, len(wire))
			}
			if ok && v != false {
				t.Fatalf("parallel_tool_calls = %v", v)
			}
		})
	}
}
