package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/newsdigest/internal/claude"
	"github.com/fjaeckel/newsdigest/internal/config"
	"github.com/fjaeckel/newsdigest/internal/digest"
	"github.com/fjaeckel/newsdigest/internal/store"
)

// stubBackend returns canned JSON so the tests never touch the network or the API.
type stubBackend struct {
	response string
	lastReq  claude.Request
}

func (s *stubBackend) Name() string { return "stub" }

func (s *stubBackend) Complete(_ context.Context, req claude.Request) (string, error) {
	s.lastReq = req
	return s.response, nil
}

const feedXML = `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Test Feed</title>
  <item>
    <title>Council approves the new bridge</title>
    <link>https://example.com/bridge</link>
    <description>&lt;p&gt;After &amp;quot;years&amp;quot; of debate.&lt;/p&gt;</description>
    <pubDate>%s</pubDate>
  </item>
  <item>
    <title>Bundesliga matchday roundup</title>
    <link>https://example.com/sports</link>
    <description>Goals galore.</description>
    <pubDate>%s</pubDate>
  </item>
</channel></rss>`

type harness struct {
	srv     *Server
	store   *store.Store
	backend *stubBackend
	date    string
	dataDir string
}

func setup(t *testing.T, response string) harness {
	t.Helper()

	now := time.Now().UTC().Format(time.RFC1123Z)
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, strings.Replace(feedXML, "%s", now, 2))
	}))
	t.Cleanup(feedSrv.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "feeds.yaml")
	cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
		"exclude:\n  topics:\n    - Sports of any kind.\n  keywords:\n    - bundesliga\n" +
		"feeds:\n  - name: Test Feed\n    url: " + feedSrv.URL + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	st, err := store.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	backend := &stubBackend{response: response}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: backend, Log: log}

	srv, err := New(cfg, st, gen, log)
	if err != nil {
		t.Fatal(err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	if _, err := gen.Run(context.Background(), date); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return harness{srv: srv, store: st, backend: backend, date: date, dataDir: dataDir}
}

const goodResponse = `{"topics":[
  {"headline":"New bridge approved","summary":"The council signed off after years of debate.",
   "tag":"local","importance":"high","source_indexes":[0]}
]}`

func TestGenerateAndRender(t *testing.T) {
	h := setup(t, goodResponse)
	srv, st, backend, date := h.srv, h.store, h.backend, h.date

	// The sports item must be filtered out before Claude ever sees it.
	if strings.Contains(strings.ToLower(backend.lastReq.User), "bundesliga") {
		t.Error("sports item reached the prompt despite the keyword filter")
	}
	if !strings.Contains(backend.lastReq.System, "Sports of any kind.") {
		t.Error("exclusion topic missing from the system prompt")
	}

	d, err := st.LoadDigest(date)
	if err != nil || d == nil {
		t.Fatalf("digest not stored: %v", err)
	}
	if len(d.Topics) != 1 {
		t.Fatalf("want 1 topic, got %d", len(d.Topics))
	}
	if got := d.Topics[0].Sources; len(got) != 1 || got[0].URL != "https://example.com/bridge" {
		t.Fatalf("sources not mapped back to the real item: %+v", got)
	}
	if !d.Topics[0].Important {
		t.Error("importance:high did not survive")
	}
	if d.Stats.FilteredOut != 1 {
		t.Errorf("want 1 pre-filtered item, got %d", d.Stats.FilteredOut)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"New bridge approved",
		"years of debate",
		`<span id="unread-count">1</span> unread of 1`,
		"example.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "Bundesliga") {
		t.Error("sports leaked into the rendered page")
	}
}

func TestMarkRead(t *testing.T) {
	hn := setup(t, goodResponse)
	st, date := hn.store, hn.date
	h := hn.srv.Handler()

	d, _ := st.LoadDigest(date)
	id := d.Topics[0].ID

	body := `{"date":"` + date + `","id":"` + id + `","read":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/read", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/read = %d: %s", rec.Code, rec.Body.String())
	}

	var res struct{ Unread int }
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Unread != 0 {
		t.Errorf("want 0 unread, got %d", res.Unread)
	}
	if !st.ReadSet(date)[id] {
		t.Error("read mark not persisted")
	}

	// Re-open the store from disk: read state must survive a restart.
	reopened, err := store.New(hn.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.ReadSet(date)[id] {
		t.Error("read mark did not survive a store reopen")
	}

	// Toggling back clears it.
	body = `{"date":"` + date + `","id":"` + id + `","read":false}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/read", strings.NewReader(body)))
	if st.ReadSet(date)[id] {
		t.Error("unread toggle did not clear the mark")
	}
}

func TestAuthToken(t *testing.T) {
	t.Setenv("DIGEST_TOKEN", "s3cret")
	h := setup(t, goodResponse).srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated request = %d, want 403", rec.Code)
	}

	// /healthz stays open so container health checks work.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}

	// ?t= sets a cookie and redirects.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?t=s3cret", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("token request = %d, want 302", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Value != "s3cret" {
		t.Fatal("no auth cookie set")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie request = %d, want 200", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	hn := setup(t, goodResponse)

	rec := httptest.NewRecorder()
	hn.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d", rec.Code)
	}

	var res struct {
		Generating bool   `json:"generating"`
		Latest     string `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Generating {
		t.Error("nothing is running, but status says generating")
	}
	if res.Latest != hn.date {
		t.Errorf("latest = %q, want %q", res.Latest, hn.date)
	}
}

func TestExtractJSONTolerance(t *testing.T) {
	cases := map[string]string{
		"plain":        `{"topics":[]}`,
		"fenced":       "```json\n{\"topics\":[]}\n```",
		"chatty":       "Here you go:\n\n{\"topics\":[]}\n\nHope that helps!",
		"brace in str": `{"topics":[{"headline":"a } b"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := claude.ExtractJSON(in)
			if err != nil {
				t.Fatalf("ExtractJSON(%q): %v", in, err)
			}
			var v any
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Fatalf("extracted invalid JSON %q: %v", got, err)
			}
		})
	}
}

func TestHallucinatedSourceIndexIsDropped(t *testing.T) {
	resp := `{"topics":[{"headline":"Something","summary":"x","tag":"t",
	  "importance":"normal","source_indexes":[0,99,-1]}]}`
	hn := setup(t, resp)

	d, _ := hn.store.LoadDigest(hn.date)
	if len(d.Topics[0].Sources) != 1 {
		t.Fatalf("out-of-range indexes were not dropped: %+v", d.Topics[0].Sources)
	}
}
