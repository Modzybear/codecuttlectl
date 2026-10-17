package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/Modzybear/codecuttlectl/internal/cuttlebone/v1"
	"github.com/Modzybear/codecuttlectl/internal/pluginhost"
	"github.com/Modzybear/codecuttlectl/internal/pluginkit"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

const parallelFixture = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Title: Go release notes\nURL: https://go.dev/doc/devel/release\nExcerpts: Current Go release documentation."},{"type":"text","text":"Second source excerpt."}]}}`

func TestParallelExecute(t *testing.T) {
	t.Setenv("CODECUTTLECTL_WEB_SEARCH_PROVIDER", "parallel")
	t.Setenv("EXA_API_KEY", "must-not-be-forwarded")
	p := newWebSearchPlugin()
	var session string
	calls := 0
	p.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != parallelSearchURL || req.Method != http.MethodPost || req.Header.Get("User-Agent") != parallelUserAgent {
			t.Fatalf("unexpected request: %s %s, headers=%v", req.Method, req.URL, req.Header)
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("X-API-Key") != "" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Fatalf("unexpected headers: %v", req.Header)
		}
		var call struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int    `json:"id"`
			Method  string `json:"method"`
			Params  struct {
				Name      string `json:"name"`
				Arguments struct {
					Objective     string   `json:"objective"`
					SearchQueries []string `json:"search_queries"`
					SessionID     string   `json:"session_id"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
			t.Fatal(err)
		}
		args := call.Params.Arguments
		if call.JSONRPC != "2.0" || call.ID != 1 || call.Method != "tools/call" || call.Params.Name != "web_search" || args.Objective != "current Go release notes" || !reflect.DeepEqual(args.SearchQueries, []string{args.Objective}) || len(args.SessionID) != 32 {
			t.Fatalf("unexpected call: %+v", call)
		}
		if session != "" && session != args.SessionID {
			t.Fatal("session identifier changed between calls")
		}
		session = args.SessionID
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(parallelFixture))}, nil
	})
	for range 2 {
		resp, err := p.Execute(context.Background(), &pb.ExecuteRequest{Input: `{"query":"current Go release notes"}`})
		if err != nil || resp.IsError || !strings.Contains(resp.Output, "https://go.dev/") || !strings.Contains(resp.Output, "Second source excerpt") || resp.Metadata["provider"] != "parallel" {
			t.Fatalf("search output: %+v, %v", resp, err)
		}
	}
	if calls != 2 {
		t.Fatalf("requests=%d", calls)
	}
}

func TestSearchProviderSelection(t *testing.T) {
	for _, provider := range []string{"", "exa", "parallel", "typo"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("CODECUTTLECTL_WEB_SEARCH_PROVIDER", provider)
			p := newWebSearchPlugin()
			desc, err := p.Describe(context.Background())
			if provider == "typo" {
				if err == nil {
					t.Fatal("invalid provider accepted")
				}
				resp, _ := p.Execute(context.Background(), &pb.ExecuteRequest{Input: `{"query":"docs"}`})
				if !resp.IsError {
					t.Fatal("invalid provider executed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal([]byte(desc.InputSchema), &schema); err != nil {
				t.Fatal(err)
			}
			_, hasType := schema.Properties["type"]
			_, hasCount := schema.Properties["num_results"]
			if _, ok := schema.Properties["query"]; !ok || hasType != (provider != "parallel") || hasCount != (provider != "parallel") {
				t.Fatalf("wrong schema for %q: %s", provider, desc.InputSchema)
			}
			if provider == "parallel" {
				return
			}
			t.Setenv("EXA_API_KEY", "saved-exa-key")
			for _, input := range []string{`{"query":"docs"}`, `{"query":"docs","num_results":50,"type":"deep"}`} {
				p.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host != "mcp.exa.ai" || req.URL.Query().Get("exaApiKey") != "saved-exa-key" {
						t.Fatalf("Exa selection lost: %s", req.URL)
					}
					var call struct {
						Params struct {
							Name      string        `json:"name"`
							Arguments exaSearchArgs `json:"arguments"`
						} `json:"params"`
					}
					if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
						t.Fatal(err)
					}
					wantCount, wantType := 8, "auto"
					if strings.Contains(input, "deep") {
						wantCount, wantType = 20, "deep"
					}
					if call.Params.Name != "web_search_exa" || call.Params.Arguments.NumResults != wantCount || call.Params.Arguments.Type != wantType {
						t.Fatalf("Exa controls changed: %+v", call)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(parallelFixture))}, nil
				})
				resp, err := p.Execute(context.Background(), &pb.ExecuteRequest{Input: input})
				if err != nil || resp.IsError || resp.Metadata["type"] == "" || resp.Metadata["num_results"] == "" {
					t.Fatalf("Exa response changed: %+v %v", resp, err)
				}
			}
		})
	}
}

func TestParallelFailures(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"http", "rate limited", "status 429", 429},
		{"rpc", `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad query"}}`, "bad query", 200},
		{"tool", `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"quota exceeded"}]}}`, "quota exceeded", 200},
		{"malformed", `{"jsonrpc":`, "parsing Parallel", 200},
		{"missing", `{"jsonrpc":"2.0","id":1}`, "missing Parallel search result", 200},
		{"empty", `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, "no text content", 200},
		{"wrong_id", `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`, "invalid Parallel JSON-RPC", 200},
		{"oversized", strings.Repeat("x", parallelResponseLimit+1), "exceeds", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &webSearchPlugin{provider: "parallel", httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			resp, err := p.Execute(context.Background(), &pb.ExecuteRequest{Input: `{"query":"docs"}`})
			if err != nil || !resp.IsError || resp.Output != "" || !strings.Contains(resp.ErrorMessage, tc.want) {
				t.Fatalf("failure lost: %+v %v", resp, err)
			}
		})
	}
	p := &webSearchPlugin{provider: "parallel"}
	for _, input := range []string{`{`, `{}`, `{"query":"docs","type":"fast"}`, `{"query":"docs","num_results":0}`} {
		resp, err := p.Execute(context.Background(), &pb.ExecuteRequest{Input: input})
		if err != nil || !resp.IsError {
			t.Fatalf("invalid input accepted: %s, %+v %v", input, resp, err)
		}
	}
}

func TestParallelSSE(t *testing.T) {
	for _, body := range []string{
		"event: message\ndata: " + parallelFixture + "\n\n",
		": keepalive\r\n\r\ndata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\r\n\r\nevent: message\r\ndata: " + parallelFixture + "\r\n\r\n",
		"data: " + strings.Replace(parallelFixture, `,"result":`, ",\ndata: \"result\":", 1) + "\n\n",
	} {
		p := &webSearchPlugin{provider: "parallel", httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		resp, err := p.Execute(context.Background(), &pb.ExecuteRequest{Input: `{"query":"docs"}`})
		if err != nil || resp.IsError || !strings.Contains(resp.Output, "Second source excerpt") {
			t.Fatalf("SSE output: %+v %v", resp, err)
		}
	}
}

func TestParallelCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			p := &webSearchPlugin{provider: "parallel", httpClient: &http.Client{Transport: rerouteTransport(server.URL)}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if timeout {
				p.httpClient.Timeout = 50 * time.Millisecond
			}
			done := make(chan *pb.ExecuteResponse, 1)
			go func() { resp, _ := p.Execute(ctx, &pb.ExecuteRequest{Input: `{"query":"docs"}`}); done <- resp }()
			<-started
			if !timeout {
				cancel()
			}
			select {
			case resp := <-done:
				want := "context canceled"
				if timeout {
					want = "Client.Timeout"
				}
				if !resp.IsError || !strings.Contains(resp.ErrorMessage, want) {
					t.Fatalf("context outcome: %+v", resp)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request did not stop")
			}
		})
	}
}

// Preserve and check the production destination before routing tests locally.
func rerouteTransport(destination string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != parallelSearchURL || req.Header.Get("User-Agent") != parallelUserAgent {
			return nil, fmt.Errorf("wrong Parallel destination or User-Agent")
		}
		clone := req.Clone(req.Context())
		clone.URL, _ = url.Parse(destination)
		return http.DefaultTransport.RoundTrip(clone)
	})
}

func TestMain(m *testing.M) {
	if destination := os.Getenv("CODECUTTLECTL_TEST_PARALLEL_FIXTURE"); destination != "" {
		p := newWebSearchPlugin()
		p.httpClient.Transport = rerouteTransport(destination)
		pluginkit.Serve(p)
		return
	}
	os.Exit(m.Run())
}

func TestParallelPluginHostDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil || call.Params.Name != "web_search" {
			t.Errorf("unexpected dispatch: %+v %v", call, err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, parallelFixture)
	}))
	defer server.Close()
	t.Setenv("CODECUTTLECTL_WEB_SEARCH_PROVIDER", "parallel")
	t.Setenv("CODECUTTLECTL_TEST_PARALLEL_FIXTURE", server.URL)
	t.Setenv("EXA_API_KEY", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Start this test executable in plugin mode through the normal loader.
	wrapper := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "cuttlebone-web-search"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	manager := pluginhost.NewManager(false)
	defer manager.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := manager.DiscoverPlugins(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.PluginNames(), []string{"websearch"}) {
		t.Fatalf("plugin not discovered: %v", manager.PluginNames())
	}
	result, err := manager.ExecuteFull(ctx, "websearch", json.RawMessage(`{"query":"current Go release notes"}`), dir)
	if err != nil || result.IsError || result.Metadata["provider"] != "parallel" || !strings.Contains(result.Output, "https://go.dev/") {
		t.Fatalf("loader lost search output: %+v %v", result, err)
	}
	result, err = manager.ExecuteFull(ctx, "websearch", json.RawMessage(`{"query":"docs","type":"deep"}`), dir)
	if err == nil || !result.IsError {
		t.Fatalf("unsupported control accepted by host: %+v %v", result, err)
	}
}
