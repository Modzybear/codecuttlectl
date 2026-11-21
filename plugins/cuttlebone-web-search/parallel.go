package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const parallelSearchURL = "https://search.parallel.ai/mcp"
const parallelUserAgent = "codecuttlectl/parallel-search"
const parallelResponseLimit = 512 * 1024

type parallelSearchInput struct {
	Query string `json:"query" jsonschema:"required" jsonschema_description:"The search query: describe the current information or documentation to find"`
}

func newWebSearchPlugin() *webSearchPlugin {
	p := &webSearchPlugin{
		httpClient: &http.Client{Timeout: 25 * time.Second},
		provider:   strings.ToLower(strings.TrimSpace(os.Getenv("CODECUTTLECTL_WEB_SEARCH_PROVIDER"))),
	}
	if p.provider == "parallel" {
		// One stable identifier per plugin process, reused across searches.
		var id [16]byte
		rand.Read(id[:])
		p.parallelSessionID = hex.EncodeToString(id[:])
	}
	return p
}

func (p *webSearchPlugin) validateProvider() error {
	switch p.provider {
	case "", "exa", "parallel":
		return nil
	default:
		return fmt.Errorf("unsupported CODECUTTLECTL_WEB_SEARCH_PROVIDER %q: use exa or parallel", p.provider)
	}
}

func (p *webSearchPlugin) parallelSearch(ctx context.Context, query string) (string, error) {
	args := struct {
		Objective     string   `json:"objective"`
		SearchQueries []string `json:"search_queries"`
		SessionID     string   `json:"session_id,omitempty"`
	}{query, []string{query}, p.parallelSessionID}
	body, err := json.Marshal(mcpRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: mcpParams{Name: "web_search", Arguments: args},
	})
	if err != nil {
		return "", fmt.Errorf("marshaling Parallel request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parallelSearchURL, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("building Parallel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", parallelUserAgent)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("executing Parallel request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("Parallel returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, parallelResponseLimit+1))
	if err != nil {
		return "", fmt.Errorf("reading Parallel response: %w", err)
	}
	if len(body) > parallelResponseLimit {
		return "", fmt.Errorf("Parallel response exceeds %d bytes", parallelResponseLimit)
	}
	return parseParallelResponse(body)
}

// Parallel may return JSON or a Streamable HTTP SSE response. Ignore SSE
// notifications and require the response matching this request's JSON-RPC id.
func parseParallelResponse(body []byte) (string, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		return parallelContent([]byte(trimmed))
	}
	scanner := bufio.NewScanner(strings.NewReader(trimmed))
	scanner.Buffer(make([]byte, 4096), parallelResponseLimit+1)
	var data []string
	consume := func() (string, bool, error) {
		if len(data) == 0 {
			return "", false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			return "", true, fmt.Errorf("invalid Parallel SSE data: %w", err)
		}
		if len(envelope.ID) == 0 {
			return "", false, nil
		}
		text, err := parallelContent([]byte(payload))
		return text, true, err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if text, done, err := consume(); done {
				return text, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading Parallel SSE: %w", err)
	}
	if text, done, err := consume(); done {
		return text, err
	}
	return "", fmt.Errorf("missing Parallel search response")
}

func parallelContent(body []byte) (string, error) {
	var resp struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      int       `json:"id"`
		Error   *mcpError `json:"error"`
		Result  *struct {
			Content []mcpContent `json:"content"`
			IsError bool         `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parsing Parallel response: %w", err)
	}
	if resp.JSONRPC != "2.0" || resp.ID != 1 {
		return "", fmt.Errorf("invalid Parallel JSON-RPC response")
	}
	if resp.Error != nil {
		return "", fmt.Errorf("Parallel error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		return "", fmt.Errorf("missing Parallel search result")
	}
	var texts []string
	for _, content := range resp.Result.Content {
		if content.Type == "text" && content.Text != "" {
			texts = append(texts, content.Text)
		}
	}
	text := strings.Join(texts, "\n\n")
	if resp.Result.IsError {
		return "", fmt.Errorf("Parallel search error: %s", text)
	}
	if text == "" {
		return "", fmt.Errorf("no text content in Parallel search result")
	}
	return text, nil
}
