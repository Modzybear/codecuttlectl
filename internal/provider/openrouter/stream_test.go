package openrouter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/provider"
)

func collectStream(t *testing.T, data string) []provider.StreamEvent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, data)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := New(Config{BaseURL: server.URL})
	client.waitRetry = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	var events []provider.StreamEvent
	for event := range client.ConverseStream(ctx, provider.Request{}) {
		events = append(events, event)
	}
	return events
}

func sse(payloads ...string) string {
	return "data: " + strings.Join(payloads, "\n\ndata: ") + "\n\n"
}

func TestStreamInterleavedToolsAreSerialized(t *testing.T) {
	events := collectStream(t, sse(
		`{"choices":[{"delta":{"reasoning":"planning"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"grep","arguments":"{\"pattern\":"}},{"index":0,"id":"a","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}},{"index":1,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
		`[DONE]`,
	))
	var current string
	var calls []string
	var args strings.Builder
	stops, usages := 0, 0
	for _, event := range events {
		switch e := event.(type) {
		case provider.ToolUseStartEvent:
			if current != "" {
				t.Fatal("overlapping starts cannot be represented by the provider event contract")
			}
			current = e.ToolUseID + ":" + e.Name
			args.Reset()
		case provider.ToolInputDeltaEvent:
			if current == "" {
				t.Fatal("arguments without a tool start")
			}
			args.WriteString(e.Delta)
		case provider.ToolUseStopEvent:
			calls = append(calls, current+":"+args.String())
			current = ""
		case provider.MessageStopEvent:
			stops++
		case provider.UsageEvent:
			usages++
		case provider.StreamErrorEvent:
			t.Fatalf("unexpected error: %v", e.Err)
		}
	}
	want := `a:read_file:{"path":"a.go"}|b:grep:{"pattern":"x"}`
	if got := strings.Join(calls, "|"); got != want || stops != 1 || usages != 1 {
		t.Fatalf("calls=%q stops=%d usages=%d; want %q and one stop/usage", got, stops, usages, want)
	}
}

func TestStreamFailuresAreNotSuccessfulTurns(t *testing.T) {
	for name, data := range map[string]string{
		"reasoning EOF":         sse(`{"choices":[{"delta":{"reasoning":"planning"}}]}`),
		"done without finish":   sse(`[DONE]`),
		"upstream error":        sse(`{"error":{"code":503,"message":"upstream unavailable"}}`),
		"malformed JSON":        sse(`{bad json`, `{"choices":[{"finish_reason":"stop"}]}`, `[DONE]`),
		"scanner limit":         "data: " + strings.Repeat("x", 300*1024),
		"unfinished tool":       sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"edit_file","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`),
		"tool without identity": sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`),
		"length limited tools":  sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"edit_file","arguments":"{}"}}]},"finish_reason":"length"}]}`, `[DONE]`),
		"malformed response":    sse(`{"choices":[{"delta":{"content":""},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`, `[DONE]`),
		"malformed function":    sse(`{"choices":[{"delta":{"content":""},"finish_reason":"error","native_finish_reason":"MALFORMED_FUNCTION_CALL"}]}`, `[DONE]`),
		"finish error":          sse(`{"choices":[{"delta":{"content":""},"finish_reason":"error"}]}`, `[DONE]`),
	} {
		t.Run(name, func(t *testing.T) {
			errors := 0
			for _, event := range collectStream(t, data) {
				switch event.(type) {
				case provider.StreamErrorEvent:
					errors++
				case provider.MessageStopEvent, provider.ToolUseStartEvent, provider.ToolUseStopEvent:
					t.Fatalf("failed stream emitted executable/success event %T", event)
				}
			}
			if errors != 1 {
				t.Fatalf("errors=%d, want exactly one", errors)
			}
		})
	}
}

func TestStreamUsageSnapshotsDoNotEraseOrDoubleCount(t *testing.T) {
	events := collectStream(t, sse(
		`{"usage":{"prompt_tokens":100,"completion_tokens":2}}`,
		`{"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}]}`,
		`{"usage":{"completion_tokens":10}}`,
		`{"usage":{}}`, `[DONE]`,
	))
	var usages []provider.UsageEvent
	for _, event := range events {
		if e, ok := event.(provider.UsageEvent); ok {
			usages = append(usages, e)
		}
	}
	if len(usages) != 1 || usages[0].InputTokens != 100 || usages[0].OutputTokens != 10 {
		t.Fatalf("usage=%+v, want one authoritative merged snapshot", usages)
	}
}

func TestStreamRepeatedTerminalChoiceWithUsage(t *testing.T) {
	for _, finish := range []string{"stop", "tool_calls"} {
		t.Run(finish, func(t *testing.T) {
			first := `{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`
			if finish == "tool_calls" {
				first = `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
			}
			metadata := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":%q}],"usage":{"prompt_tokens":123,"completion_tokens":4}}`, finish)
			stops, usages, tools := 0, 0, 0
			for _, event := range collectStream(t, sse(first, metadata, metadata, `[DONE]`)) {
				switch e := event.(type) {
				case provider.MessageStopEvent:
					stops++
					if e.StopReason != finish {
						t.Fatalf("stop=%q, want %q", e.StopReason, finish)
					}
				case provider.UsageEvent:
					usages++
					if e.InputTokens != 123 || e.OutputTokens != 4 {
						t.Fatalf("unexpected usage: %+v", e)
					}
				case provider.ToolUseStopEvent:
					tools++
				case provider.StreamErrorEvent:
					t.Fatal(e.Err)
				}
			}
			wantTools := 0
			if finish == "tool_calls" {
				wantTools = 1
			}
			if stops != 1 || usages != 1 || tools != wantTools {
				t.Fatalf("stops=%d usages=%d tools=%d", stops, usages, tools)
			}
		})
	}
}

func TestStreamRejectsContentAfterCompletion(t *testing.T) {
	for _, delta := range []string{
		`{"delta":{"content":"late text"}}`,
		`{"delta":{"reasoning":"late reasoning"}}`,
		`{"delta":{"tool_calls":[{"index":0}]}}`,
		`{"delta":{},"finish_reason":"length"}`,
	} {
		errors := 0
		for _, event := range collectStream(t, sse(`{"choices":[{"finish_reason":"stop"}]}`, `{"choices":[`+delta+`]}`, `[DONE]`)) {
			switch event.(type) {
			case provider.StreamErrorEvent:
				errors++
			case provider.MessageStopEvent, provider.ToolUseStartEvent:
				t.Fatalf("invalid stream emitted success event: %T", event)
			}
		}
		if errors != 1 {
			t.Fatalf("errors=%d, want 1", errors)
		}
	}
}

func TestStreamUsagePresence(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		count         int
		unknown       bool
	}{
		{"absent", `{}`, 0, false},
		{"null", `{"usage":null}`, 0, false},
		{"empty", `{"usage":{}}`, 0, false},
		{"output only", `{"usage":{"completion_tokens":7}}`, 1, true},
		{"explicit zero", `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			for _, event := range collectStream(t, sse(`{"choices":[{"finish_reason":"stop"}]}`, tc.payload, `[DONE]`)) {
				if e, ok := event.(provider.UsageEvent); ok {
					count++
					if e.InputTokensUnknown != tc.unknown {
						t.Fatalf("unknown=%v, want %v", e.InputTokensUnknown, tc.unknown)
					}
				}
				if e, ok := event.(provider.StreamErrorEvent); ok {
					t.Fatal(e.Err)
				}
			}
			if count != tc.count {
				t.Fatalf("usage events=%d, want %d", count, tc.count)
			}
		})
	}
}

func TestStreamReasoningOnlyWithFinishIsValid(t *testing.T) {
	stops := 0
	for _, event := range collectStream(t, sse(`{"choices":[{"delta":{"reasoning":"plan"},"finish_reason":"stop"}]}`, `[DONE]`)) {
		switch e := event.(type) {
		case provider.MessageStopEvent:
			stops++
		case provider.StreamErrorEvent:
			t.Fatal(e.Err)
		}
	}
	if stops != 1 {
		t.Fatalf("stops=%d, want 1", stops)
	}
}

func TestStreamSSEFraming(t *testing.T) {
	data := ": keepalive\r\n\r\ndata:{\"choices\":[\r\ndata: {\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata:[DONE]\r\n\r\n"
	text, stops := "", 0
	for _, event := range collectStream(t, data) {
		switch e := event.(type) {
		case provider.TextDeltaEvent:
			text += e.Text
		case provider.MessageStopEvent:
			stops++
		case provider.StreamErrorEvent:
			t.Fatal(e.Err)
		}
	}
	if text != "ok" || stops != 1 {
		t.Fatalf("text=%q stops=%d", text, stops)
	}
}

// A canceled consumer must release the HTTP body even if it stops draining events.
func TestStreamCancelWithFullEventQueue(t *testing.T) {
	closed := make(chan struct{})
	c := New(Config{})
	c.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		data := strings.Repeat(sse(`{"choices":[{"delta":{"content":"x"}}]}`), 200)
		return &http.Response{StatusCode: 200, Body: &notifyingBody{Reader: strings.NewReader(data), closed: closed}, Header: make(http.Header)}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := c.ConverseStream(ctx, provider.Request{})
	deadline := time.After(2 * time.Second)
	for len(ch) < cap(ch) {
		select {
		case <-deadline:
			t.Fatal("event queue did not fill")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		// Drain to clean up the broken implementation before failing.
		go func() {
			for range ch {
			}
		}()
		t.Fatal("canceled producer is blocked on an abandoned event queue")
	}
}

func TestStreamReasoningDetailsAccumulationAndParallelEncrypted(t *testing.T) {
	events := collectStream(t, sse(
		`{"choices":[{"delta":{"reasoning":"step 1 ","reasoning_details":[{"type":"reasoning.text","index":0,"format":"google-gemini-v1","text":"step 1 "}]}}]}`,
		`{"choices":[{"delta":{"reasoning":"step 2","reasoning_details":[{"type":"reasoning.text","index":0,"format":"google-gemini-v1","text":"step 2","signature":"sig_txt"}]}}]}`,
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_a","index":0,"format":"google-gemini-v1","data":"enc_a"}],"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_b","index":0,"format":"google-gemini-v1","data":"enc_b"}],"tool_calls":[{"index":1,"id":"call_b","function":{"name":"grep","arguments":"{\"pattern\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))

	var reasoningText strings.Builder
	var sigEvents []provider.ReasoningSignatureEvent
	var sigIdx, firstToolIdx, msgStopIdx = -1, -1, -1

	for i, ev := range events {
		switch e := ev.(type) {
		case provider.ReasoningDeltaEvent:
			reasoningText.WriteString(e.Text)
		case provider.ReasoningSignatureEvent:
			sigEvents = append(sigEvents, e)
			if sigIdx == -1 {
				sigIdx = i
			}
		case provider.ToolUseStartEvent:
			if firstToolIdx == -1 {
				firstToolIdx = i
			}
		case provider.MessageStopEvent:
			msgStopIdx = i
		case provider.StreamErrorEvent:
			t.Fatalf("unexpected error: %v", e.Err)
		}
	}

	// Plaintext reasoning must not be double-emitted from reasoning_details.
	if reasoningText.String() != "step 1 step 2" {
		t.Fatalf("reasoning text=%q, want %q", reasoningText.String(), "step 1 step 2")
	}
	if len(sigEvents) != 1 {
		t.Fatalf("expected 1 ReasoningSignatureEvent, got %d", len(sigEvents))
	}
	if sigIdx == -1 || firstToolIdx == -1 || msgStopIdx == -1 || !(sigIdx < firstToolIdx && firstToolIdx < msgStopIdx) {
		t.Fatalf("unexpected event order: sig=%d tool=%d stop=%d", sigIdx, firstToolIdx, msgStopIdx)
	}
	details := sigEvents[0].Details
	if len(details) != 3 {
		t.Fatalf("expected 3 details (1 merged text + 2 distinct parallel encrypted), got %+v", details)
	}
	if details[0].Type != "reasoning.text" || details[0].Text != "step 1 step 2" || details[0].Signature != "sig_txt" {
		t.Errorf("unexpected merged reasoning.text: %+v", details[0])
	}
	if details[1].Type != "reasoning.encrypted" || details[1].ID != "call_a" || details[1].Data != "enc_a" {
		t.Errorf("unexpected first encrypted detail: %+v", details[1])
	}
	if details[2].Type != "reasoning.encrypted" || details[2].ID != "call_b" || details[2].Data != "enc_b" {
		t.Errorf("unexpected second encrypted detail: %+v", details[2])
	}
}

func TestStreamEncryptedOnlyReasoningStillRetriesMalformed(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_bad","index":0,"format":"google-gemini-v1","data":"enc_bad"}]},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`,
				`[DONE]`,
			))
			return
		}
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"recovered"},"finish_reason":"stop"}]}`,
			`[DONE]`,
		))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	c.waitRetry = func(context.Context, time.Duration) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var text strings.Builder
	var retries int
	for ev := range c.ConverseStream(ctx, provider.Request{}) {
		switch e := ev.(type) {
		case provider.RetryEvent:
			retries++
		case provider.TextDeltaEvent:
			text.WriteString(e.Text)
		case provider.StreamErrorEvent:
			t.Fatalf("unexpected error after retry: %v", e.Err)
		}
	}
	if attempts != 2 || retries != 1 || text.String() != "recovered" {
		t.Fatalf("attempts=%d retries=%d text=%q; want 2, 1, recovered", attempts, retries, text.String())
	}
}

func TestStreamUsageCostPreserved(t *testing.T) {
	events := collectStream(t, sse(
		`{"choices":[{"delta":{"content":"hello"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0015}}`,
		`[DONE]`,
	))

	var gotUsage *provider.UsageEvent
	for _, event := range events {
		if u, ok := event.(provider.UsageEvent); ok {
			gotUsage = &u
		}
	}
	if gotUsage == nil {
		t.Fatal("expected UsageEvent")
	}
	if gotUsage.InputTokens != 100 || gotUsage.OutputTokens != 20 || gotUsage.CostUSD != 0.0015 {
		t.Fatalf("unexpected usage: %+v (want 100, 20, 0.0015)", gotUsage)
	}
}

func TestStreamUsagePromptTokensDetailsCached(t *testing.T) {
	events := collectStream(t, sse(
		`{"choices":[{"delta":{"content":"cached reply"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10339,"completion_tokens":60,"cost":0.0042,"prompt_tokens_details":{"cached_tokens":10318,"cache_write_tokens":0}}}`,
		`[DONE]`,
	))

	var gotUsage *provider.UsageEvent
	for _, event := range events {
		if u, ok := event.(provider.UsageEvent); ok {
			gotUsage = &u
		}
	}
	if gotUsage == nil {
		t.Fatal("expected UsageEvent")
	}
	if gotUsage.InputTokens != 21 || gotUsage.CacheReadTokens != 10318 || gotUsage.CacheWriteTokens != 0 || gotUsage.OutputTokens != 60 || gotUsage.CostUSD != 0.0042 || gotUsage.InputTokensUnknown {
		t.Fatalf("unexpected usage: %+v (want in=21, cacheRead=10318, cacheWrite=0, out=60, cost=0.0042, unknown=false)", gotUsage)
	}
}

func TestStreamUsageNegativeCachedTokensRejected(t *testing.T) {
	ch := make(chan provider.StreamEvent, 10)
	err := parseSSEStream(context.Background(), strings.NewReader(sse(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":-5}}}`,
		`[DONE]`,
	)), ch)
	if err == nil || !strings.Contains(err.Error(), "negative cached token count") {
		t.Fatalf("expected negative cached token error, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type notifyingBody struct {
	io.Reader
	closed chan struct{}
}

func (b *notifyingBody) Close() error { close(b.closed); return nil }
