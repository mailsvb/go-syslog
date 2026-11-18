// Package mcpserver exposes the go-syslog log directory to AI agents as a
// read-only MCP server over streamable HTTP.
package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mailsvb/go-syslog/internal/query"
)

// Path is where the MCP endpoint is served.
const Path = "/mcp"

const instructions = `Read-only access to the syslog messages collected by go-syslog.

Data: every received message is stored with its receive time (UTC, ms) per sender IP and UTC hour. Senders configured with -split-by-tag (for example Docker hosts) are split further by syslog tag, so a source is "<ip>" or "<ip>/<tag>". Messages are the raw datagrams, starting with the syslog <PRI>, and are kept for the retention period of the collector (7 days by default).

Recommended workflow:
1. list_sources to see which senders exist and when they last wrote.
2. log_stats to find where and when errors happen (counts per severity and app) before reading raw lines.
3. search_logs with a source, time range, severity and text/regex filters. Results are newest first by default; follow next_cursor for more.
4. get_context with the ref of an interesting line to read what happened around it.

Times accept RFC 3339 or a duration ago such as 15m, 6h or 2d. Keep pages small; narrow the filters instead of paging through everything.`

// New returns the MCP server with the read-only log tools.
func New(store *query.Store, version string, log *slog.Logger) *mcp.Server {
	if log == nil {
		log = slog.Default()
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "go-syslog", Title: "go-syslog logs", Version: version},
		&mcp.ServerOptions{Instructions: instructions, Logger: quiet(log)})
	t := tools{store: store, log: log}
	notOpen := false
	ann := func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &notOpen}
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sources",
		Description: "List the senders (<ip>) and tags of split senders (<ip>/<tag>) with stored logs, most recently active first, with file count, size, oldest hour and last write time. Also returns the server time (UTC).",
		Annotations: ann("List log sources"),
	}, logged(log, "list_sources", t.listSources))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "log_stats",
		Description: "Count lines per source (or per source and hour) in a time range: total lines and bytes, lines per severity, first/last receive time and the most frequent syslog tags (apps). Use this first to find where errors are before reading raw lines.",
		Annotations: ann("Log statistics"),
	}, logged(log, "log_stats", t.stats))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_logs",
		Description: "Search log lines by source, receive time range, severity, case-insensitive text and/or RE2 regex on the raw message. Newest first by default. Returns at most limit lines and about max_bytes of text; pass next_cursor back as cursor (with the same filters) for the next page. Each line has a ref for get_context.",
		Annotations: ann("Search logs"),
	}, logged(log, "search_logs", t.search))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_context",
		Description: "Return the lines before and after a line from search_logs (by its ref), in file order, from the same source and hour file.",
		Annotations: ann("Log context"),
	}, logged(log, "get_context", t.context))
	return s
}

// logged logs every tool call with its duration and error.
func logged[In, Out any](log *slog.Logger, name string, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		res, out, err := h(ctx, req, in)
		attrs := []any{"tool", name, "duration", time.Since(start).Round(time.Millisecond)}
		if err != nil {
			log.Info("tool call failed", append(attrs, "err", err)...)
		} else {
			log.Info("tool call", attrs...)
		}
		return res, out, err
	}
}

type tools struct {
	store *query.Store
	log   *slog.Logger
}

// ListSourcesIn is the input of list_sources.
type ListSourcesIn struct {
	Source string `json:"source,omitempty" jsonschema:"optional: only this <ip> (with its tags) or <ip>/<tag>"`
}

// ListSourcesOut is the result of list_sources.
type ListSourcesOut struct {
	ServerTime string         `json:"server_time"`
	Sources    []query.Source `json:"sources"`
}

func (t tools) listSources(_ context.Context, _ *mcp.CallToolRequest, in ListSourcesIn) (*mcp.CallToolResult, ListSourcesOut, error) {
	src, err := t.store.Sources(in.Source)
	if err != nil {
		return nil, ListSourcesOut{}, err
	}
	if src == nil {
		src = []query.Source{}
	}
	return nil, ListSourcesOut{ServerTime: t.store.Now().Format(time.RFC3339), Sources: src}, nil
}

// TimeRange is shared by the tools that take a time range.
type TimeRange struct {
	From string `json:"from,omitempty" jsonschema:"start of the receive time range: RFC 3339 or a duration ago like 30m, 6h, 2d (default: 24h before to)"`
	To   string `json:"to,omitempty" jsonschema:"end of the range (exclusive), same formats (default: now)"`
}

func (r TimeRange) parse(now time.Time) (time.Time, time.Time, error) {
	from, err := query.ParseTime(r.From, now)
	if err != nil {
		return from, from, err
	}
	to, err := query.ParseTime(r.To, now)
	return from, to, err
}

// StatsIn is the input of log_stats.
type StatsIn struct {
	Source string `json:"source,omitempty" jsonschema:"optional: <ip> (with its tags) or <ip>/<tag>; default all sources"`
	TimeRange
	ByHour  bool `json:"by_hour,omitempty" jsonschema:"one row per source and UTC hour instead of one per source"`
	TopApps int  `json:"top_apps,omitempty" jsonschema:"number of most frequent tags per row (default 5, max 50)"`
}

func (t tools) stats(ctx context.Context, _ *mcp.CallToolRequest, in StatsIn) (*mcp.CallToolResult, *query.StatsResult, error) {
	from, to, err := in.parse(t.store.Now())
	if err != nil {
		return nil, nil, err
	}
	res, err := t.store.Stats(ctx, query.StatsParams{
		Source: in.Source, From: from, To: to, ByHour: in.ByHour, TopApps: min(in.TopApps, 50),
	})
	return nil, res, err
}

// SearchIn is the input of search_logs.
type SearchIn struct {
	Source string `json:"source,omitempty" jsonschema:"optional: <ip> (with its tags) or <ip>/<tag>; default all sources"`
	TimeRange
	Contains  string `json:"contains,omitempty" jsonschema:"case-insensitive text the message must contain"`
	Regex     string `json:"regex,omitempty" jsonschema:"RE2 regular expression the message must match, e.g. (?i)timeout|refused"`
	Severity  string `json:"severity,omitempty" jsonschema:"only this severity or worse: emerg, alert, crit, err, warning, notice, info, debug; lines without PRI are then left out"`
	Order     string `json:"order,omitempty" jsonschema:"newest (default) or oldest"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max lines (default 50, max 500)"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"approximate max size of the returned text (default 32768, max 262144)"`
	LineBytes int    `json:"line_bytes,omitempty" jsonschema:"longer messages are clipped (default 1000)"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

func (t tools) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, *query.SearchResult, error) {
	from, to, err := in.parse(t.store.Now())
	if err != nil {
		return nil, nil, err
	}
	sev, err := query.ParseSeverity(strings.ToLower(strings.TrimSpace(in.Severity)))
	if err != nil {
		return nil, nil, err
	}
	var oldest bool
	switch strings.ToLower(in.Order) {
	case "", "newest", "desc":
	case "oldest", "asc":
		oldest = true
	default:
		return nil, nil, errors.New("order must be newest or oldest")
	}
	res, err := t.store.Search(ctx, query.SearchParams{
		Source: in.Source, From: from, To: to, Contains: in.Contains, Regex: in.Regex,
		MaxSeverity: sev, Oldest: oldest, Limit: in.Limit, MaxBytes: in.MaxBytes,
		LineBytes: in.LineBytes, Cursor: in.Cursor,
	})
	return nil, res, err
}

// ContextIn is the input of get_context.
type ContextIn struct {
	Ref       string `json:"ref" jsonschema:"ref of a line from search_logs"`
	Before    *int   `json:"before,omitempty" jsonschema:"lines before (default 10, max 200)"`
	After     *int   `json:"after,omitempty" jsonschema:"lines after (default 10, max 200)"`
	LineBytes int    `json:"line_bytes,omitempty" jsonschema:"longer messages are clipped (default 1000)"`
}

func (t tools) context(ctx context.Context, _ *mcp.CallToolRequest, in ContextIn) (*mcp.CallToolResult, *query.ContextResult, error) {
	before, after := 10, 10
	if in.Before != nil {
		before = *in.Before
	}
	if in.After != nil {
		after = *in.After
	}
	res, err := t.store.Context(ctx, in.Ref, before, after, in.LineBytes)
	return nil, res, err
}

// Handler returns the HTTP handler: the MCP endpoint at Path (stateless
// streamable HTTP with JSON responses) and GET /healthz. If token is not
// empty, the MCP endpoint requires "Authorization: Bearer <token>".
func Handler(s *mcp.Server, token string, log *slog.Logger) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: quiet(log)})
	mux := http.NewServeMux()
	mux.Handle(Path, requireToken(token, h))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// quiet returns a logger for the SDK that drops its per-request info and
// debug messages (every stateless request connects and disconnects a session).
func quiet(log *slog.Logger) *slog.Logger {
	if log == nil {
		return nil
	}
	return slog.New(warnHandler{log.Handler()})
}

type warnHandler struct{ slog.Handler }

func (h warnHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

func (h warnHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return warnHandler{h.Handler.WithAttrs(a)}
}

func (h warnHandler) WithGroup(name string) slog.Handler {
	return warnHandler{h.Handler.WithGroup(name)}
}

func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="go-syslog"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
