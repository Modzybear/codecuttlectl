# Web Search & Fetch Plugins

Two plugins provide web access capabilities:

## `websearch` — Web Search

Searches the web using [Exa's](https://exa.ai) free MCP (Model Context Protocol) endpoint by default. Returns structured results with titles, URLs, and content highlights optimized for LLM consumption. You can opt into [Parallel Search MCP](https://docs.parallel.ai/integrations/mcp/search-mcp) instead, using the same `websearch` tool.

### How it works

The plugin sends a JSON-RPC request to `https://mcp.exa.ai/mcp`, which is Exa's public MCP endpoint. No API key is required for basic usage — the endpoint is free and returns high-quality search results with extracted page content.

### Exa parameters (default backend)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `query` | string | ✅ | — | The search query |
| `num_results` | integer | — | 8 | Number of results (max 20) |
| `type` | string | — | `"auto"` | `"auto"`, `"fast"`, or `"deep"` |

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `CODECUTTLECTL_WEB_SEARCH_PROVIDER` | No | `exa` (default) or `parallel`. Set before starting the CLI; the plugin selects its backend at startup. Invalid values prevent the search plugin from loading. |
| `EXA_API_KEY` | No | Optional. Provides higher rate limits and priority access. |

### Example usage (from the agent)

```
websearch({"query": "go 1.25 generics constraints", "num_results": 5})
```

### Response format

Returns a text block with structured results:
```
Title: Example Page Title
URL: https://example.com/page
Published: 2026-01-15T...
Highlights:
Relevant content extracted from the page...

---

Title: Another Result
URL: https://another.com
...
```

### Parallel backend (opt in)

Parallel provides free web search with source URLs and relevant excerpts, with no account or API key required. Anonymous searches use Fast mode and are subject to free-tier rate limits. This backend calls `web_search` at `https://search.parallel.ai/mcp` over HTTP.

After building the CLI and plugins with `make all` as described in the [README](../README.md#installation), start it with:

```bash
CODECUTTLECTL_WEB_SEARCH_PROVIDER=parallel ./bin/codecuttlectl -plugin-dir ./bin/plugins
```

The agent still calls `websearch`, with one required string parameter, `query`:

```text
websearch({"query": "Go release notes latest stable"})
```

The query is sent as Parallel's search objective and a single keyword query. All returned text blocks are passed back to the agent, including source URLs and excerpts. Parallel manages result count and search mode for anonymous requests, so Exa's `num_results` and `type` parameters are omitted from the Parallel tool schema and rejected if supplied. Search failures, including rate limits, are returned as tool errors without switching backends or retrying automatically. Requests retain the plugin's 25-second HTTP timeout and support cancellation.

The Parallel backend does not read `EXA_API_KEY` or send credentials. To use Exa again, start the CLI with the variable unset or set to `exa`. The separate `webfetch` plugin continues to retrieve URLs directly, including URLs found with Parallel.

---

## `webfetch` — URL Content Retrieval

Fetches a URL and converts the content to readable text or markdown. Handles HTML pages by stripping non-content elements (scripts, styles, navigation) and preserving meaningful structure.

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `url` | string | ✅ | — | Full URL (must start with `http://` or `https://`) |
| `format` | string | — | `"text"` | `"text"`, `"markdown"`, or `"html"` |
| `max_size` | integer | — | 50000 | Max characters to return (max 200000) |

### Format options

- **`text`** — Strips all HTML, extracts readable text. Best for general reading.
- **`markdown`** — Converts HTML to Markdown, preserving headings, code blocks, lists, bold/italic.
- **`html`** — Returns raw HTML. Use only when you need the actual markup.

### Limits

- Maximum raw response: 5MB
- Maximum output: 200,000 characters
- Request timeout: 30 seconds
- Maximum redirects: 5

### Example usage

```
webfetch({"url": "https://pkg.go.dev/golang.org/x/net/html", "format": "markdown"})
```

---

## Architecture

```
┌─────────────┐     JSON-RPC/POST      ┌──────────────┐
│  websearch  │ ───────────────────────▶│ mcp.exa.ai   │
│  (plugin)   │ ◀──────────────────────│ (free MCP)   │
└─────────────┘     Search results      └──────────────┘

With CODECUTTLECTL_WEB_SEARCH_PROVIDER=parallel, websearch instead calls
https://search.parallel.ai/mcp using the same HTTP JSON-RPC transport.

┌─────────────┐     HTTP GET            ┌──────────────┐
│  webfetch   │ ───────────────────────▶│  Any URL     │
│  (plugin)   │ ◀──────────────────────│              │
└─────────────┘     HTML → text/md      └──────────────┘
```

Both plugins run as isolated gRPC subprocesses. They use Go's standard `net/http` client — no external dependencies beyond the standard library and the pluginkit framework.

## When to use which

| Scenario | Tool |
|----------|------|
| "What's the latest version of X?" | `websearch` |
| "Find docs for Y" | `websearch` → then `webfetch` on the best URL |
| "Read this GitHub README" | `webfetch` |
| "What are the arguments to Z function?" | `websearch` (often enough) or `webfetch` if you have the doc URL |
| User pastes a URL | `webfetch` |
