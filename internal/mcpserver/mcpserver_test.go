package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mailsvb/go-syslog/internal/query"
	"github.com/mailsvb/go-syslog/internal/writer"
)

var now = time.Date(2026, 11, 18, 13, 30, 0, 0, time.UTC)

func setup(t *testing.T, token string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	write := func(source string, ts time.Time, msg string) {
		ip, tag, _ := strings.Cut(source, "/")
		p := filepath.Join(dir, ip, tag, ts.Format(writer.HourLayout)+writer.FileExt)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(writer.FormatLine(ts, []byte(msg))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		ts := now.Add(-time.Hour + time.Duration(i)*time.Minute)
		sev := 6
		if i%10 == 9 {
			sev = 3
		}
		write("192.168.21.5", ts, fmt.Sprintf("<%d>Nov 18 12:00:00 ccu3 ReGaHss: line %d", 8+sev, i))
		write("127.0.0.1/nginx", ts, fmt.Sprintf("<%d>1 - - nginx - - - request %d", 24+sev, i))
	}
	store, err := query.Open(dir, query.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	store.SetClock(func() time.Time { return now })
	t.Cleanup(func() { store.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(Handler(New(store, "1.2.3", log), token, log))
	t.Cleanup(ts.Close)
	return ts
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	hc := &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + Path, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return res
}

func TestTools(t *testing.T) {
	ts := setup(t, "s3cret")
	cs := connect(t, ts.URL, "s3cret")

	if got := cs.InitializeResult().ServerInfo.Version; got != "1.2.3" {
		t.Errorf("server version %q", got)
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
	if strings.Join(names, ",") != "get_context,list_sources,log_stats,search_logs" {
		t.Errorf("tools = %v", names)
	}

	var src ListSourcesOut
	call(t, cs, "list_sources", nil, &src)
	if len(src.Sources) != 2 || src.ServerTime != "2026-11-18T13:30:00Z" {
		t.Fatalf("list_sources = %+v", src)
	}

	var st query.StatsResult
	call(t, cs, "log_stats", map[string]any{"from": "2h"}, &st)
	if len(st.Rows) != 2 || st.Rows[0].Lines != 30 || st.Rows[0].Severity["err"] != 3 {
		t.Fatalf("log_stats = %+v", st)
	}

	var sr query.SearchResult
	call(t, cs, "search_logs", map[string]any{"source": "192.168.21.5", "severity": "error", "limit": 2}, &sr)
	if len(sr.Lines) != 2 || !strings.HasSuffix(sr.Lines[0].Message, "line 29") || sr.NextCursor == "" {
		t.Fatalf("search_logs = %+v", sr)
	}
	var sr2 query.SearchResult
	call(t, cs, "search_logs", map[string]any{"source": "192.168.21.5", "severity": "error", "limit": 2, "cursor": sr.NextCursor}, &sr2)
	if len(sr2.Lines) != 1 || !strings.HasSuffix(sr2.Lines[0].Message, "line 9") {
		t.Fatalf("page 2 = %+v", sr2)
	}

	var cr query.ContextResult
	call(t, cs, "get_context", map[string]any{"ref": sr.Lines[1].Ref, "before": 1, "after": 0}, &cr)
	if len(cr.Lines) != 2 || !cr.Lines[1].Target || !strings.HasSuffix(cr.Lines[0].Message, "line 18") {
		t.Fatalf("get_context = %+v", cr)
	}

	for name, args := range map[string]map[string]any{
		"search_logs":  {"source": "../../etc"},
		"get_context":  {"ref": "../../etc/passwd:0"},
		"log_stats":    {"from": "yesterday"},
		"list_sources": {"source": "x/y/z"},
	} {
		if res := call(t, cs, name, args, nil); !res.IsError {
			t.Errorf("%s(%v): expected a tool error", name, args)
		}
	}
}

func TestAuth(t *testing.T) {
	ts := setup(t, "s3cret")
	for _, h := range []string{"", "Bearer wrong", "s3cret"} {
		req, _ := http.NewRequest("POST", ts.URL+Path, strings.NewReader(`{}`))
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d", h, resp.StatusCode)
		}
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

func TestNoToken(t *testing.T) {
	ts := setup(t, "")
	cs := connect(t, ts.URL, "")
	var src ListSourcesOut
	call(t, cs, "list_sources", map[string]any{}, &src)
	if len(src.Sources) != 2 {
		t.Fatalf("list_sources = %+v", src)
	}
}
