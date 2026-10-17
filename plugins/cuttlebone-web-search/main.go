// cuttlebone-web-search provides the websearch tool using Exa's free MCP
// endpoint by default, or an opt-in Parallel Search MCP backend.
// URL retrieval is provided separately by cuttlebone-web-fetch.
//
// Environment variables:
//   - EXA_API_KEY: Optional. Provides higher rate limits for Exa search.
//   - CODECUTTLECTL_WEB_SEARCH_PROVIDER: exa (default) or parallel.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	pb "github.com/Modzybear/codecuttlectl/internal/cuttlebone/v1"
	"github.com/Modzybear/codecuttlectl/internal/pluginkit"
	"github.com/Modzybear/codecuttlectl/internal/pluginkit/schema"
	"github.com/Modzybear/codecuttlectl/internal/pluginkit/types"
)

//go:embed skills/*
var skillFS embed.FS

type webSearchPlugin struct {
	httpClient        *http.Client
	provider          string
	parallelSessionID string
}

// --- Tool Input Structs ---

type searchInput struct {
	Query      string        `json:"query" jsonschema:"required" jsonschema_description:"The search query to execute"`
	NumResults types.FlexInt `json:"num_results,omitempty" jsonschema_description:"Number of search results to return (default: 8, max: 20)"`
	Type       string        `json:"type,omitempty" jsonschema:"enum=auto,enum=fast,enum=deep" jsonschema_description:"Search type: 'auto' (balanced, default), 'fast' (quick results), 'deep' (comprehensive)"`
}

type fetchInput struct {
	URL     string        `json:"url" jsonschema:"required" jsonschema_description:"The URL to fetch content from. Must start with http:// or https://"`
	MaxSize types.FlexInt `json:"max_size,omitempty" jsonschema_description:"Maximum response size in bytes to process (default: 500000, max: 2000000)"`
}

// --- Exa MCP Types ---

type mcpRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int       `json:"id"`
	Method  string    `json:"method"`
	Params  mcpParams `json:"params"`
}

type mcpParams struct {
	Name      string      `json:"name"`
	Arguments interface{} `json:"arguments"`
}

type exaSearchArgs struct {
	Query                string `json:"query"`
	Type                 string `json:"type"`
	NumResults           int    `json:"numResults"`
	Livecrawl            string `json:"livecrawl"`
	ContextMaxCharacters int    `json:"contextMaxCharacters,omitempty"`
}

type mcpResponse struct {
	Result *mcpResult `json:"result,omitempty"`
	Error  *mcpError  `json:"error,omitempty"`
}

type mcpResult struct {
	Content []mcpContent `json:"content"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- Plugin Implementation ---

func (p *webSearchPlugin) Describe(ctx context.Context) (*pb.DescribeResponse, error) {
	if err := p.validateProvider(); err != nil {
		return nil, err
	}
	inputSchema := schema.MustSchema(&searchInput{})
	description := "Search the web and fetch URL content. Performs real-time web searches via Exa and retrieves/converts web page content to readable text. Use for accessing information beyond the knowledge cutoff, researching APIs, or reading documentation."
	if p.provider == "parallel" {
		inputSchema = schema.MustSchema(&parallelSearchInput{})
		description = "Search the web via Parallel for current information, API references, and documentation. Returns source URLs and relevant excerpts."
	}
	return &pb.DescribeResponse{
		Name:           "websearch",
		Description:    description,
		InputSchema:    inputSchema,
		LlmContextHint: `Use websearch to find current information, documentation, API references, or anything beyond your training data. The current year is ` + fmt.Sprintf("%d", time.Now().Year()) + `. Always use the current year when searching for recent information.`,
		Version:        "1.1.0",
		Capabilities: &pb.ToolCapabilities{
			SupportsCancellation: true,
			MaxTimeoutSeconds:    30,
		},
		Skills: []*pb.Skill{
			pluginkit.EmbedSkill(skillFS, "skills/web_research.md",
				"web_research_workflow", "on_request", 30),
		},
	}, nil
}

func (p *webSearchPlugin) Execute(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	if err := p.validateProvider(); err != nil {
		return &pb.ExecuteResponse{IsError: true, ErrorMessage: err.Error()}, nil
	}
	var params searchInput
	if err := json.Unmarshal([]byte(req.Input), &params); err != nil {
		return &pb.ExecuteResponse{
			IsError:      true,
			ErrorMessage: fmt.Sprintf("parsing input: %v", err),
		}, nil
	}

	if params.Query == "" {
		return &pb.ExecuteResponse{
			IsError:      true,
			ErrorMessage: "query is required",
		}, nil
	}
	if p.provider == "parallel" {
		// Exa's per-call controls have no equivalent on the anonymous Parallel
		// endpoint. Reject them rather than silently changing their meaning.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(req.Input), &fields)
		for _, name := range []string{"type", "num_results"} {
			if _, ok := fields[name]; ok {
				return &pb.ExecuteResponse{IsError: true, ErrorMessage: name + " is only supported by the Exa backend"}, nil
			}
		}
		result, err := p.parallelSearch(ctx, params.Query)
		if err != nil {
			return &pb.ExecuteResponse{IsError: true, ErrorMessage: fmt.Sprintf("search failed: %v", err)}, nil
		}
		return &pb.ExecuteResponse{Output: result, Metadata: map[string]string{"query": params.Query, "provider": "parallel"}}, nil
	}

	numResults := params.NumResults.Int()
	if numResults <= 0 {
		numResults = 8
	}
	if numResults > 20 {
		numResults = 20
	}

	searchType := params.Type
	if searchType == "" {
		searchType = "auto"
	}

	result, err := p.exaSearch(ctx, params.Query, searchType, numResults)
	if err != nil {
		return &pb.ExecuteResponse{
			IsError:      true,
			ErrorMessage: fmt.Sprintf("search failed: %v", err),
		}, nil
	}

	return &pb.ExecuteResponse{
		Output: result,
		Metadata: map[string]string{
			"query":       params.Query,
			"num_results": fmt.Sprintf("%d", numResults),
			"type":        searchType,
		},
	}, nil
}

// exaSearch calls the Exa MCP endpoint with the given query.
func (p *webSearchPlugin) exaSearch(ctx context.Context, query, searchType string, numResults int) (string, error) {
	// Build the Exa MCP URL (with optional API key)
	exaURL := "https://mcp.exa.ai/mcp"
	if key := os.Getenv("EXA_API_KEY"); key != "" {
		exaURL = fmt.Sprintf("https://mcp.exa.ai/mcp?exaApiKey=%s", key)
	}

	// Build the JSON-RPC request
	args := exaSearchArgs{
		Query:      query,
		Type:       searchType,
		NumResults: numResults,
		Livecrawl:  "fallback",
	}

	mcpReq := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: mcpParams{
			Name:      "web_search_exa",
			Arguments: args,
		},
	}

	body, err := json.Marshal(mcpReq)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", exaURL, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("exa returned status %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	// Parse the response — handle both direct JSON and SSE (data: prefix) formats
	return parseExaResponse(respBody)
}

// parseExaResponse handles both direct JSON responses and SSE-wrapped responses
// from the Exa MCP endpoint.
func parseExaResponse(body []byte) (string, error) {
	trimmed := strings.TrimSpace(string(body))

	// Try direct JSON parse first
	if strings.HasPrefix(trimmed, "{") {
		var mcpResp mcpResponse
		if err := json.Unmarshal([]byte(trimmed), &mcpResp); err == nil {
			return extractMCPContent(mcpResp)
		}
	}

	// Try SSE format: look for "data: {...}" lines
	for _, line := range strings.Split(trimmed, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		payload = strings.TrimSpace(payload)
		if !strings.HasPrefix(payload, "{") {
			continue
		}
		var mcpResp mcpResponse
		if err := json.Unmarshal([]byte(payload), &mcpResp); err == nil {
			return extractMCPContent(mcpResp)
		}
	}

	// Last resort: the entire body might be SSE with "event:" + "data:" pairs
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimSpace(payload)
			if strings.HasPrefix(payload, "{") {
				var mcpResp mcpResponse
				if err := json.Unmarshal([]byte(payload), &mcpResp); err == nil {
					return extractMCPContent(mcpResp)
				}
			}
		}
	}

	return "", fmt.Errorf("could not parse Exa response (length=%d)", len(body))
}

func extractMCPContent(resp mcpResponse) (string, error) {
	if resp.Error != nil {
		return "", fmt.Errorf("exa error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil || len(resp.Result.Content) == 0 {
		return "No search results found. Try a different query.", nil
	}

	// Find the text content block
	for _, content := range resp.Result.Content {
		if content.Type == "text" && content.Text != "" {
			return content.Text, nil
		}
	}

	return "No text content in search results.", nil
}

func main() {
	pluginkit.Serve(newWebSearchPlugin())
}
