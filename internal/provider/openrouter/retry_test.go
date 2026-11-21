package openrouter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Modzybear/codecuttlectl/internal/provider"
)

func TestRateLimitRecovery(t *testing.T) {
	for _, streamingError := range []bool{false, true} {
		for _, exhaust := range []bool{false, true} {
			t.Run(fmt.Sprintf("SSE=%v/exhaust=%v", streamingError, exhaust), func(t *testing.T) {
				var requests atomic.Int32
				var bodies []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(body))
					n := requests.Add(1)
					if exhaust || n == 1 {
						w.Header().Set("Retry-After", "1")
						if streamingError {
							fmt.Fprint(w, sse(`{"error":{"code":"429","message":"secret upstream text"}}`))
						} else {
							w.WriteHeader(http.StatusTooManyRequests)
							fmt.Fprint(w, "secret upstream text")
						}
						return
					}
					fmt.Fprint(w, sse(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`, `{"usage":{"prompt_tokens":10,"completion_tokens":1}}`, `[DONE]`))
				}))
				defer server.Close()
				client := New(Config{BaseURL: server.URL})
				client.waitRetry = func(ctx context.Context, delay time.Duration) error {
					if delay != time.Second {
						t.Errorf("Retry-After ignored: %s", delay)
					}
					return ctx.Err()
				}
				retries, failures, stops, usages := 0, 0, 0, 0
				text := ""
				for event := range client.ConverseStream(context.Background(), provider.Request{Messages: []provider.Message{provider.BuildUserTextMessage("test")}}) {
					switch e := event.(type) {
					case provider.RetryEvent:
						retries++
						if e.Attempt != retries || e.MaxRetries != 2 {
							t.Errorf("bad retry event: %+v", e)
						}
					case provider.StreamErrorEvent:
						failures++
						if !provider.IsThrottleError(e.Err) || provider.IsTransientStreamError(e.Err) || strings.Contains(e.Err.Error(), "secret") {
							t.Errorf("unsafe classification/diagnostic: %v", e.Err)
						}
					case provider.TextDeltaEvent:
						text += e.Text
					case provider.MessageStopEvent:
						stops++
					case provider.UsageEvent:
						usages++
					}
				}
				if exhaust {
					if requests.Load() != 3 || retries != 2 || failures != 1 || stops != 0 || usages != 0 {
						t.Fatalf("requests=%d retry=%d errors=%d stop=%d usage=%d", requests.Load(), retries, failures, stops, usages)
					}
				} else if requests.Load() != 2 || retries != 1 || failures != 0 || stops != 1 || usages != 1 || text != "OK" {
					t.Fatalf("requests=%d retry=%d errors=%d stop=%d usage=%d text=%q", requests.Load(), retries, failures, stops, usages, text)
				}
				for _, body := range bodies {
					if body != bodies[0] {
						t.Fatal("retry mutated request/history")
					}
				}
			})
		}
	}
}

func TestRateLimitAfterContentIsNotRetried(t *testing.T) {
	for _, delta := range []string{
		`{"content":"partial"}`, `{"reasoning":"thought"}`,
		`{"tool_calls":[{"index":0,"id":"t","function":{"name":"write_file","arguments":"{"}}]}`,
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			fmt.Fprint(w, sse(`{"choices":[{"delta":`+delta+`}]}`, `{"error":{"code":429}}`))
		}))
		client := New(Config{BaseURL: server.URL})
		count := 0
		for event := range client.ConverseStream(context.Background(), provider.Request{}) {
			switch e := event.(type) {
			case provider.RetryEvent, provider.ToolUseStartEvent, provider.MessageStopEvent, provider.UsageEvent:
				t.Fatalf("unsafe event after partial failure: %T", e)
			case provider.StreamErrorEvent:
				count++
				var limit *rateLimitError
				if !errors.As(e.Err, &limit) || !limit.partial || provider.IsTransientStreamError(e.Err) {
					t.Fatalf("partial retry not suppressed: %v", e.Err)
				}
			}
		}
		server.Close()
		if calls.Load() != 1 || count != 1 {
			t.Fatalf("requests=%d errors=%d", calls.Load(), count)
		}
	}
}

func TestRateLimitWithContentInSameFrame(t *testing.T) {
	ch := make(chan provider.StreamEvent, 8)
	err := parseSSEStream(context.Background(), strings.NewReader(sse(`{"choices":[{"delta":{"content":"partial"}}],"error":{"code":429}}`)), ch)
	var limit *rateLimitError
	if !errors.As(err, &limit) || !limit.partial {
		t.Fatalf("co-located content must suppress retry: %v", err)
	}
}

func TestRateLimitCancelBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := New(Config{BaseURL: server.URL}).ConverseStream(ctx, provider.Request{})
	select {
	case event := <-ch:
		if _, ok := event.(provider.RetryEvent); !ok {
			t.Fatalf("want retry status, got %T", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no retry event")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("event after canceled backoff")
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("extra request after cancel: %d", calls.Load())
	}
}

func TestRetryAfterPolicy(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"2", 2 * time.Second}, {"0", 0}, {"-1", 0}, {"invalid", 0},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"9223372036854775807", maxRateLimitWait + time.Second},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("Retry-After %q = %s, want %s", tc.value, got, tc.want)
		}
	}
	if _, retry := rateLimitDelay(&rateLimitError{retryAfter: 3 * time.Minute}, 0); retry {
		t.Fatal("must not shorten server wait to fit retry budget")
	}
	for attempt, want := range []time.Duration{30 * time.Second, 60 * time.Second} {
		if delay, retry := rateLimitDelay(&rateLimitError{}, attempt); !retry || delay != want {
			t.Fatalf("backoff=%s retry=%v", delay, retry)
		}
	}
}

func TestUnaryRateLimitRecovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL})
	client.waitRetry = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	response, err := client.Converse(context.Background(), provider.Request{})
	if err != nil || response.Content != "OK" || calls.Load() != 2 {
		t.Fatalf("response=%+v err=%v requests=%d", response, err, calls.Load())
	}
}

func TestMalformedResponseStreamRecovery(t *testing.T) {
	for _, nativeReason := range []string{"MALFORMED_RESPONSE", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL"} {
		for _, exhaust := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exhaust=%v", nativeReason, exhaust), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					n := requests.Add(1)
					if exhaust || n == 1 {
						finish := "stop"
						if nativeReason == "MALFORMED_FUNCTION_CALL" {
							finish = "error"
						}
						payload := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"","role":"assistant"},"finish_reason":%q,"native_finish_reason":%q}]}`, finish, nativeReason)
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
						return
					}
					fmt.Fprint(w, sse(`{"choices":[{"delta":{"content":"Recovered!"},"finish_reason":"stop"}]}`, `{"usage":{"prompt_tokens":100,"completion_tokens":5}}`, `[DONE]`))
				}))
				defer server.Close()

				client := New(Config{BaseURL: server.URL})
				client.waitRetry = func(ctx context.Context, delay time.Duration) error {
					if delay < time.Second {
						t.Errorf("unexpected short delay: %s", delay)
					}
					return ctx.Err()
				}

				retries, failures, stops, usages := 0, 0, 0, 0
				text := ""
				for event := range client.ConverseStream(context.Background(), provider.Request{}) {
					switch e := event.(type) {
					case provider.RetryEvent:
						retries++
						if e.Attempt != retries || e.MaxRetries != maxMalformedRetries || e.Reason != nativeReason {
							t.Errorf("bad malformed retry event: %+v", e)
						}
					case provider.StreamErrorEvent:
						failures++
						var mErr *malformedResponseError
						if !errors.As(e.Err, &mErr) || mErr.nativeReason != nativeReason {
							t.Errorf("expected malformedResponseError with %s, got: %v", nativeReason, e.Err)
						}
					case provider.TextDeltaEvent:
						text += e.Text
					case provider.MessageStopEvent:
						stops++
					case provider.UsageEvent:
						usages++
					}
				}

				if exhaust {
					if requests.Load() != 4 || retries != 3 || failures != 1 || stops != 0 || usages != 0 {
						t.Fatalf("exhaust: requests=%d retries=%d failures=%d stops=%d usages=%d", requests.Load(), retries, failures, stops, usages)
					}
				} else {
					if requests.Load() != 2 || retries != 1 || failures != 0 || stops != 1 || usages != 1 || text != "Recovered!" {
						t.Fatalf("recovered: requests=%d retries=%d failures=%d stops=%d usages=%d text=%q", requests.Load(), retries, failures, stops, usages, text)
					}
				}
			})
		}
	}
}

func TestErrorFinishWithPolicyNativeReasonNotRetried(t *testing.T) {
	for _, policyReason := range []string{"SAFETY", "RECITATION", "PROHIBITED_CONTENT"} {
		t.Run(policyReason, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				payload := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"","role":"assistant"},"finish_reason":"error","native_finish_reason":%q}]}`, policyReason)
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
			}))
			defer server.Close()

			client := New(Config{BaseURL: server.URL})
			retries, failures := 0, 0
			var streamErr error
			for event := range client.ConverseStream(context.Background(), provider.Request{}) {
				switch e := event.(type) {
				case provider.RetryEvent:
					retries++
				case provider.StreamErrorEvent:
					failures++
					streamErr = e.Err
				}
			}
			if requests.Load() != 1 || retries != 0 || failures != 1 {
				t.Fatalf("requests=%d retries=%d failures=%d, want 1 request and 0 retries", requests.Load(), retries, failures)
			}
			if streamErr == nil || !strings.Contains(streamErr.Error(), policyReason) || strings.Contains(streamErr.Error(), "malformed") {
				t.Fatalf("expected policy error containing %s and not malformed, got: %v", policyReason, streamErr)
			}
		})
	}
}

func TestMalformedResponseWithBufferedToolCallsRetriesCleanly(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			// First attempt sends partial tool calls then MALFORMED_FUNCTION_CALL
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"a.go\""}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"error","native_finish_reason":"MALFORMED_FUNCTION_CALL"}]}`,
				`[DONE]`,
			))
			return
		}
		// Second attempt succeeds
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL})
	client.waitRetry = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	toolStarts, toolStops, retries, failures := 0, 0, 0, 0
	for event := range client.ConverseStream(context.Background(), provider.Request{}) {
		switch event.(type) {
		case provider.RetryEvent:
			retries++
		case provider.ToolUseStartEvent:
			toolStarts++
		case provider.ToolUseStopEvent:
			toolStops++
		case provider.StreamErrorEvent:
			failures++
		}
	}
	if requests.Load() != 2 || retries != 1 || failures != 0 || toolStarts != 1 || toolStops != 1 {
		t.Fatalf("requests=%d retries=%d failures=%d toolStarts=%d toolStops=%d", requests.Load(), retries, failures, toolStarts, toolStops)
	}
}

func TestMalformedResponseCancelBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(`{"choices":[{"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`, `[DONE]`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := New(Config{BaseURL: server.URL}).ConverseStream(ctx, provider.Request{})
	select {
	case event := <-ch:
		if retryEv, ok := event.(provider.RetryEvent); !ok || retryEv.Reason != "MALFORMED_RESPONSE" {
			t.Fatalf("want malformed retry status, got %T (%+v)", event, event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no retry event")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("event after canceled backoff")
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("extra request after cancel: %d", calls.Load())
	}
}

func TestMalformedResponseRepeatedTerminalFrameAndUsage(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			// Chunk 1 has finish_reason: stop, Chunk 2 supplies native reason + usage
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`,
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}],"usage":{"prompt_tokens":93691,"completion_tokens":348}}`,
				`[DONE]`,
			))
			return
		}
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`,
			`{"usage":{"prompt_tokens":93691,"completion_tokens":10}}`,
			`[DONE]`,
		))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL})
	client.waitRetry = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	var usages []provider.UsageEvent
	retries, stops := 0, 0
	for event := range client.ConverseStream(context.Background(), provider.Request{}) {
		switch e := event.(type) {
		case provider.RetryEvent:
			retries++
		case provider.UsageEvent:
			usages = append(usages, e)
		case provider.MessageStopEvent:
			stops++
		}
	}
	// Attempt 1 emits UsageEvent (billed tokens), Attempt 2 emits UsageEvent + StopEvent
	if requests.Load() != 2 || retries != 1 || stops != 1 || len(usages) != 2 {
		t.Fatalf("requests=%d retries=%d stops=%d usages=%d", requests.Load(), retries, stops, len(usages))
	}
	if usages[0].InputTokens != 93691 || usages[0].OutputTokens != 348 || usages[0].InputTokensUnknown {
		t.Fatalf("failed attempt usage not preserved: %+v", usages[0])
	}
	if usages[1].InputTokens != 93691 || usages[1].OutputTokens != 10 {
		t.Fatalf("recovered usage not preserved: %+v", usages[1])
	}
}

func TestUnaryDecodeErrorWrapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{invalid json`)
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL})
	_, err := client.Converse(context.Background(), provider.Request{})
	if err == nil || !strings.Contains(err.Error(), "openrouter: decoding response:") {
		t.Fatalf("expected wrapped decode error, got: %v", err)
	}
}
