package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Modzybear/codecuttlectl/internal/pluginhost"
	"github.com/Modzybear/codecuttlectl/internal/provider/openrouter"
)

func sse(payloads ...string) string {
	return "data: " + strings.Join(payloads, "\n\ndata: ") + "\n\n"
}

func TestRateLimitCancelAndStaleEvents(t *testing.T) {
	for _, quit := range []bool{false, true} {
		t.Run(fmt.Sprintf("quit=%v", quit), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					fmt.Fprint(w, `{"data":[]}`)
					return
				}
				requests.Add(1)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(429)
			}))
			defer server.Close()
			m := New(Config{PluginMgr: pluginhost.NewManager(false)})
			m.llmProvider = openrouter.New(openrouter.Config{BaseURL: server.URL})
			m.lastCallInputTokens = 123
			m.submitMessage("test")
			defer m.stopModelStream()
			ctx := m.streamContext
			generation := m.streamGeneration
			ch := m.streamCh
			event := m.readNextStreamEvent()()
			updated, _ := m.Update(event)
			m = updated.(Model)
			if !strings.Contains(m.renderInput(), "Rate limited (429)") || !m.streaming {
				t.Fatal("retry status not displayed")
			}
			if m.lastCallInputTokens != 123 || len(m.history) != 1 {
				t.Fatal("retry changed context or history")
			}
			if quit {
				updated, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			} else {
				updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
				m = updated.(Model)
				updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
			}
			m = updated.(Model)
			if ctx.Err() != context.Canceled {
				t.Fatal("UI did not cancel actual provider context")
			}
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("unexpected event after cancellation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("provider backoff not canceled")
			}
			if requests.Load() != 1 {
				t.Fatal("retry issued after cancellation")
			}
			before := len(m.history)
			for _, stale := range []tea.Msg{StreamTextMsg{Text: "stale"}, StreamDoneMsg{}, StreamUsageMsg{InputTokens: 999}} {
				updated, cmd := m.Update(streamEnvelope{Generation: generation, Message: stale})
				m = updated.(Model)
				if cmd != nil || len(m.history) != before || m.lastCallInputTokens != 123 || m.streamBuf.Len() != 0 {
					t.Fatal("queued canceled event changed state")
				}
			}
		})
	}
}

func TestMalformedRetryCancelAndStaleEvents(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(`{"choices":[{"delta":{},"finish_reason":"stop","native_finish_reason":"MALFORMED_RESPONSE"}]}`, `[DONE]`))
	}))
	defer server.Close()

	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.llmProvider = openrouter.New(openrouter.Config{BaseURL: server.URL})
	m.lastCallInputTokens = 456
	m.submitMessage("test malformed cancel")
	defer m.stopModelStream()

	ctx := m.streamContext
	generation := m.streamGeneration
	ch := m.streamCh

	// Read retry event
	event := m.readNextStreamEvent()()
	updated, _ := m.Update(event)
	m = updated.(Model)

	if !strings.Contains(m.renderInput(), "Upstream MALFORMED_RESPONSE: retry 1/3 in") || !m.streaming {
		t.Fatalf("malformed retry status not displayed: %q", m.renderInput())
	}
	if m.lastCallInputTokens != 456 || len(m.history) != 1 {
		t.Fatalf("retry changed context or history: ctx=%d history=%d", m.lastCallInputTokens, len(m.history))
	}

	// Press double-Esc to cancel
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(Model)

	if ctx.Err() != context.Canceled {
		t.Fatal("UI did not cancel actual provider context")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected event after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider backoff not canceled")
	}
	if requests.Load() != 1 {
		t.Fatalf("extra request after cancel: %d", requests.Load())
	}

	// Stale events from canceled generation are safely ignored
	before := len(m.history)
	for _, stale := range []tea.Msg{StreamTextMsg{Text: "stale"}, StreamDoneMsg{}, StreamUsageMsg{InputTokens: 999}} {
		updated, cmd := m.Update(streamEnvelope{Generation: generation, Message: stale})
		m = updated.(Model)
		if cmd != nil || len(m.history) != before || m.lastCallInputTokens != 456 || m.streamBuf.Len() != 0 {
			t.Fatal("queued canceled event changed state")
		}
	}
}

func TestMalformedErrorAndEmptyResponseInTUI(t *testing.T) {
	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.streaming = true
	m.lastCallInputTokens = 789

	// A failed attempt emits UsageEvent with InputTokensUnknown (preserves lastCallInputTokens)
	usageMsg := StreamUsageMsg{
		InputTokens:        0,
		OutputTokens:       15,
		InputTokensUnknown: true,
	}
	updated, _ := m.Update(usageMsg)
	m = updated.(Model)
	if m.lastCallInputTokens != 789 || m.totalOutputTokens != 15 {
		t.Fatalf("failed usage clobbered context: ctx=%d totalOut=%d", m.lastCallInputTokens, m.totalOutputTokens)
	}

	// Error message updates UI and resets streaming without mutating history
	updated, _ = m.Update(StreamErrorMsg{Err: errors.New("openrouter: upstream malformed response (MALFORMED_RESPONSE); retry limit reached")})
	m = updated.(Model)
	if m.streaming {
		t.Fatal("expected streaming false on error")
	}
	if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].content, "MALFORMED_RESPONSE") {
		t.Fatalf("error not rendered in messages: %+v", m.messages)
	}

	// Empty StreamDoneMsg displays diagnostic
	m.streaming = true
	m.streamEmitted = false
	updated, _ = m.Update(StreamDoneMsg{})
	m = updated.(Model)
	if len(m.messages) < 2 || !strings.Contains(m.messages[len(m.messages)-1].content, "empty response") {
		t.Fatalf("empty response diagnostic not rendered: %+v", m.messages)
	}
}

func TestTransientConnectionResetStreamAutoRetriesInTUI(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// Emit partial text then abrupt EOF (simulating mid-stream connection drop)
			fmt.Fprint(w, sse(`{"choices":[{"delta":{"content":"partial discarded "}}]}`))
			return
		}
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"recovered response"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		))
	}))
	defer server.Close()

	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.llmProvider = openrouter.New(openrouter.Config{BaseURL: server.URL})
	m.transientRetryDelayFn = func(int, error) time.Duration { return time.Millisecond }
	m.submitMessage("hello")
	defer m.stopModelStream()

	// Drain attempt 1 (partial text followed by unexpected EOF -> StreamErrorMsg schedules backoff retry)
	var retryCmd tea.Cmd
	for i := 0; i < 20 && m.streaming; i++ {
		if m.streamCh == nil {
			break
		}
		ev := m.readNextStreamEvent()()
		var cmd tea.Cmd
		updated, cmd := m.Update(ev)
		m = updated.(Model)
		if m.streamCh == nil && cmd != nil {
			retryCmd = cmd
			break
		}
	}

	if retryCmd == nil {
		t.Fatal("expected backoff retry command to be scheduled after transient stream drop")
	}
	if !strings.Contains(m.renderInput(), "Upstream transient network error: retry 1/3") {
		t.Fatalf("expected transient retry countdown in input bar, got: %q", m.renderInput())
	}

	// Execute backoff timer -> fires StreamServerRetryFireMsg -> launches attempt 2
	fireMsg := retryCmd()
	updated, _ := m.Update(fireMsg)
	m = updated.(Model)

	for i := 0; i < 20 && m.streaming; i++ {
		if m.streamCh == nil {
			break
		}
		ev := m.readNextStreamEvent()()
		updated, _ := m.Update(ev)
		m = updated.(Model)
	}

	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts after transient stream drop, got %d", attempts.Load())
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].content != "recovered response" {
		t.Fatalf("expected clean 'recovered response' without partial prefix, got %+v", m.messages)
	}
}

func TestTransientRetryCancelDuringBackoff(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(`{"choices":[{"delta":{"content":"partial "}}]}`))
	}))
	defer server.Close()

	m := New(Config{PluginMgr: pluginhost.NewManager(false)})
	m.llmProvider = openrouter.New(openrouter.Config{BaseURL: server.URL})
	m.transientRetryDelayFn = func(int, error) time.Duration { return time.Millisecond }
	m.submitMessage("hello")
	defer m.stopModelStream()

	var retryCmd tea.Cmd
	for i := 0; i < 20 && m.streaming && m.streamCh != nil; i++ {
		ev := m.readNextStreamEvent()()
		updated, cmd := m.Update(ev)
		m = updated.(Model)
		if m.streamCh == nil && cmd != nil {
			retryCmd = cmd
			break
		}
	}
	if retryCmd == nil {
		t.Fatal("expected backoff retry command")
	}

	// Cancel with Esc twice during backoff
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(Model)
	if m.streaming {
		t.Fatal("expected streaming=false after double Esc during backoff")
	}

	// Firing the stale backoff timer must be ignored (no second request)
	fireMsg := retryCmd()
	updated, cmd := m.Update(fireMsg)
	m = updated.(Model)
	if cmd != nil || m.streaming || attempts.Load() != 1 {
		t.Fatalf("stale backoff timer should not launch retry: streaming=%v attempts=%d", m.streaming, attempts.Load())
	}
}
