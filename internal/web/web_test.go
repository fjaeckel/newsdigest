package web

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

	"github.com/fjaeckel/newsdigest/internal/claude"
	"github.com/fjaeckel/newsdigest/internal/config"
	"github.com/fjaeckel/newsdigest/internal/digest"
	"github.com/fjaeckel/newsdigest/internal/store"
)

// stubBackend returns canned JSON so the tests never touch the network or the
// API. Every request is kept: a run makes one call per category plus one for
// the front page, so a test that wants the category prompt has to say which.
type stubBackend struct {
	response string
	reqs     []claude.Request
}

func (s *stubBackend) Name() string { return "stub" }

func (s *stubBackend) Complete(_ context.Context, req claude.Request) (string, error) {
	s.reqs = append(s.reqs, req)
	return s.response, nil
}

// countingBackend records every call, so a test can prove each category got
// its own brief, and can fail a chosen one.
type countingBackend struct {
	response  string
	failFirst bool
	calls     int
	systems   []string
}

func (b *countingBackend) Name() string { return "counting" }

func (b *countingBackend) Complete(_ context.Context, req claude.Request) (string, error) {
	b.calls++
	b.systems = append(b.systems, req.System)
	if b.failFirst && b.calls == 1 {
		return "", fmt.Errorf("backend exploded")
	}
	return b.response, nil
}

// scriptedBackend answers each call with the next response in the script and
// repeats the last one after that, which is what lets a test drive a category
// brief and the front page that follows it independently.
type scriptedBackend struct {
	script []string
	calls  int
	reqs   []claude.Request
}

func (b *scriptedBackend) Name() string { return "scripted" }

func (b *scriptedBackend) Complete(_ context.Context, req claude.Request) (string, error) {
	b.reqs = append(b.reqs, req)
	i := min(b.calls, len(b.script)-1)
	b.calls++
	return b.script[i], nil
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
		"feeds:\n  - name: Test Feed\n    category: news\n    url: " + feedSrv.URL + "\n"
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
	catReq := backend.reqs[0]
	if strings.Contains(strings.ToLower(catReq.User), "bundesliga") {
		t.Error("sports item reached the prompt despite the keyword filter")
	}
	if !strings.Contains(catReq.System, "Sports of any kind.") {
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

	if d.Topics[0].Category != "news" {
		t.Errorf("topic category = %q, want the feed's category", d.Topics[0].Category)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/d/"+date, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /d/%s = %d", date, rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"New bridge approved",
		"years of debate",
		`<span id="unread-count">1</span> unread of 1`,
		"https://example.com/bridge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "Bundesliga") {
		t.Error("sports leaked into the rendered page")
	}
}

// Every category is briefed separately, so max_topics is a budget each one
// gets rather than a total shared across the morning.
func TestEachCategoryIsBriefedSeparately(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC1123Z)
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, strings.Replace(feedXML, "%s", now, 2))
	}))
	t.Cleanup(feedSrv.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "feeds.yaml")
	cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
		"feeds:\n" +
		"  - name: News Feed\n    category: news\n    url: " + feedSrv.URL + "\n" +
		"  - name: Bike Feed\n    category: cycling\n    url: " + feedSrv.URL + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CategoryNames(); len(got) != 2 || got[0] != "news" || got[1] != "cycling" {
		t.Fatalf("categories should follow config order, got %v", got)
	}

	st, err := store.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	backend := &countingBackend{response: goodResponse}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: backend, Log: log}

	date := time.Now().UTC().Format("2006-01-02")
	d, err := gen.Run(context.Background(), date)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// One call per category, plus the one that writes the front page from what
	// they produced.
	if backend.calls != 3 {
		t.Errorf("want one Claude call per category plus the front page, got %d", backend.calls)
	}
	if !strings.Contains(backend.systems[2], "front page") {
		t.Error("the last call was not the front page brief")
	}
	if len(d.Topics) != 2 {
		t.Fatalf("want a topic from each category, got %d", len(d.Topics))
	}
	seen := map[string]bool{}
	for _, tp := range d.Topics {
		seen[tp.Category] = true
	}
	if !seen["news"] || !seen["cycling"] {
		t.Errorf("topics not attributed to both categories: %+v", seen)
	}
	// Each brief is told which subject it is editing.
	if !strings.Contains(backend.systems[0], "cycling") && !strings.Contains(backend.systems[1], "cycling") {
		t.Error("no prompt named the cycling category")
	}
}

// One category's brief failing must not cost the others theirs.
func TestOneCategoryFailingKeepsTheRest(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC1123Z)
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, strings.Replace(feedXML, "%s", now, 2))
	}))
	t.Cleanup(feedSrv.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "feeds.yaml")
	cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
		"feeds:\n" +
		"  - name: News Feed\n    category: news\n    url: " + feedSrv.URL + "\n" +
		"  - name: Bike Feed\n    category: cycling\n    url: " + feedSrv.URL + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(cfgPath)
	st, _ := store.New(filepath.Join(dir, "data"))

	// Fail the first category, succeed on the second.
	backend := &countingBackend{response: goodResponse, failFirst: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: backend, Log: log}

	date := time.Now().UTC().Format("2006-01-02")
	d, err := gen.Run(context.Background(), date)
	if err != nil {
		t.Fatalf("one failing category took down the whole run: %v", err)
	}
	if len(d.Topics) != 1 {
		t.Fatalf("want the surviving category's topic, got %d", len(d.Topics))
	}
	if d.Topics[0].Category != "cycling" {
		t.Errorf("wrong category survived: %q", d.Topics[0].Category)
	}
	var mentioned bool
	for _, e := range d.Errors {
		if strings.Contains(e, "news") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the failure was not recorded on the digest: %v", d.Errors)
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

func TestGroupSources(t *testing.T) {
	got := groupSources([]store.Source{
		{Feed: "Guardian", URL: "https://g.com/1"},
		{Feed: "Guardian", URL: "https://g.com/2"},
		{Feed: "Tagesschau", URL: "https://t.de/1"},
		{Feed: "Guardian", URL: "https://g.com/3"},
		{Feed: "Tagesschau", URL: "https://t.de/2"},
	})

	if len(got) != 2 {
		t.Fatalf("want 2 outlets, got %d: %+v", len(got), got)
	}
	// First-seen order, and every article kept.
	if got[0].Feed != "Guardian" || len(got[0].Articles) != 3 {
		t.Errorf("group 0 = %+v", got[0])
	}
	if got[1].Feed != "Tagesschau" || len(got[1].Articles) != 2 {
		t.Errorf("group 1 = %+v", got[1])
	}
	if got[0].Articles[2].URL != "https://g.com/3" {
		t.Errorf("article order not preserved: %+v", got[0].Articles)
	}
}

// A story pulling several pieces from one outlet renders as one chip, but
// every article stays reachable inside it — they are different articles.
func TestManyArticlesFromOneOutletCollapseButStayReachable(t *testing.T) {
	hn := setup(t, goodResponse)

	d, _ := hn.store.LoadDigest(hn.date)
	d.Topics[0].Sources = nil
	for i := range 7 {
		d.Topics[0].Sources = append(d.Topics[0].Sources, store.Source{
			Feed:  "The Guardian",
			Title: fmt.Sprintf("Guardian piece %d", i),
			URL:   fmt.Sprintf("https://g.com/%d", i),
		})
	}
	d.Topics[0].Sources = append(d.Topics[0].Sources,
		store.Source{Feed: "Tagesschau", Title: "Bericht", URL: "https://t.de/x"})
	if err := hn.store.SaveDigest(d); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	hn.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/d/"+hn.date, nil))
	body := rec.Body.String()

	// One chip per outlet, not one per article.
	if n := strings.Count(body, `class="src"`); n != 2 {
		t.Errorf("want 2 outlet chips, got %d", n)
	}
	if !strings.Contains(body, `<span class="src-count">7</span>`) {
		t.Error("count badge missing for the grouped outlet")
	}
	// Nothing is dropped: all seven remain, each with its own title.
	for i := range 7 {
		if !strings.Contains(body, fmt.Sprintf("https://g.com/%d", i)) {
			t.Errorf("article %d is unreachable", i)
		}
		if !strings.Contains(body, fmt.Sprintf("Guardian piece %d", i)) {
			t.Errorf("title of article %d missing", i)
		}
	}
	if !strings.Contains(body, "https://t.de/x") {
		t.Error("the single-article outlet was dropped")
	}
}

// saveDay writes a hand-built digest so a test can span several days without
// running the generator once per day.
func saveDay(t *testing.T, st *store.Store, date string, topics ...store.Topic) {
	t.Helper()
	if err := st.SaveDigest(&store.Digest{
		Date:        date,
		GeneratedAt: time.Now().UTC(),
		Model:       "stub",
		Backend:     "stub",
		Topics:      topics,
	}); err != nil {
		t.Fatal(err)
	}
}

func topic(id, cat, headline string) store.Topic {
	return store.Topic{
		ID:       id,
		Category: cat,
		Tag:      cat,
		Headline: headline,
		Summary:  headline + " happened.",
		Sources:  []store.Source{{Feed: "Test Feed", Title: headline, URL: "https://example.com/" + id}},
	}
}

// withFeed re-attributes a topic's sources to a named outlet, which is what
// by-feed grouping keys on.
func withFeed(t store.Topic, feed string) store.Topic {
	for i := range t.Sources {
		t.Sources[i].Feed = feed
	}
	return t
}

// The point of a standing feed: an unread topic from days ago is still there,
// alongside today's, until it is read.
func TestCategoryFeedCarriesUnreadForward(t *testing.T) {
	hn := setup(t, goodResponse)
	h := hn.srv.Handler()

	saveDay(t, hn.store, "2026-08-20",
		topic("c1", "cycling", "Pogacar confirms the Giro"),
		topic("n1", "news", "Something newsworthy"))
	saveDay(t, hn.store, "2026-08-21",
		topic("c2", "cycling", "New Shimano groupset"))
	saveDay(t, hn.store, "2026-08-22",
		topic("a1", "aviation", "BasicMed rules updated"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/cycling", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /c/cycling = %d", rec.Code)
	}
	body := rec.Body.String()

	// Days-old cycling still sits in the feed next to the newest.
	for _, want := range []string{"Pogacar confirms the Giro", "New Shimano groupset"} {
		if !strings.Contains(body, want) {
			t.Errorf("category feed missing %q", want)
		}
	}
	// Other categories stay out of it entirely.
	for _, unwanted := range []string{"Something newsworthy", "BasicMed rules updated"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("category feed leaked another category: %q", unwanted)
		}
	}
	if strings.Index(body, "New Shimano groupset") > strings.Index(body, "Pogacar confirms the Giro") {
		t.Error("category feed is not newest-first")
	}
	// Each card carries its own day, so marking read hits the right digest.
	if !strings.Contains(body, `data-date="2026-08-20"`) || !strings.Contains(body, `data-date="2026-08-21"`) {
		t.Error("cards are missing their source date")
	}
	if !strings.Contains(body, "https://example.com/c1") {
		t.Error("sources missing from the category feed")
	}
}

func TestCategoryFeedMarksTheRightDayRead(t *testing.T) {
	hn := setup(t, goodResponse)
	h := hn.srv.Handler()

	saveDay(t, hn.store, "2026-08-20", topic("c1", "cycling", "Older cycling story"))
	saveDay(t, hn.store, "2026-08-21", topic("c2", "cycling", "Newer cycling story"))

	// This is the request a card on the feed makes: its own date, not today's.
	body := `{"date":"2026-08-20","id":"c1","read":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/read", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/read = %d: %s", rec.Code, rec.Body.String())
	}

	if !hn.store.ReadSet("2026-08-20")["c1"] {
		t.Error("read mark did not land on the topic's own day")
	}
	if hn.store.ReadSet("2026-08-21")["c2"] {
		t.Error("read mark bled onto another day")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/cycling", nil))
	if !strings.Contains(rec.Body.String(), `<span id="unread-count">1</span> unread of 2`) {
		t.Error("category feed unread count did not follow the mark")
	}
}

// Clearing a standing feed has to reach every day it spans, not just one.
func TestMarkCategoryClearsEveryDay(t *testing.T) {
	hn := setup(t, goodResponse)
	h := hn.srv.Handler()

	saveDay(t, hn.store, "2026-08-20",
		topic("c1", "cycling", "Old cycling"),
		topic("n1", "news", "Old news"),
		topic("f1", "fun", "Old comic"))
	saveDay(t, hn.store, "2026-08-21", topic("c2", "cycling", "New cycling"))

	// A mark on another category, on a day cycling also occupies. Clearing
	// cycling must not disturb it.
	if err := hn.store.SetRead("2026-08-20", "f1", true); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/read-category",
		strings.NewReader(`{"category":"cycling","read":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/read-category = %d: %s", rec.Code, rec.Body.String())
	}

	var res struct{ Unread int }
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Unread != 0 {
		t.Errorf("want the feed cleared, got %d unread", res.Unread)
	}
	if !hn.store.ReadSet("2026-08-20")["c1"] || !hn.store.ReadSet("2026-08-21")["c2"] {
		t.Error("clearing the feed missed a day")
	}
	// Neighbours on the same date keep their own state, in both directions.
	if hn.store.ReadSet("2026-08-20")["n1"] {
		t.Error("clearing cycling also marked news read")
	}
	if !hn.store.ReadSet("2026-08-20")["f1"] {
		t.Error("clearing cycling destroyed another category's read mark")
	}

	// And unmarking the category leaves the neighbour's mark in place too.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/read-category",
		strings.NewReader(`{"category":"cycling","read":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/read-category (unmark) = %d", rec.Code)
	}
	if hn.store.ReadSet("2026-08-20")["c1"] {
		t.Error("unmarking the category did not clear its own topics")
	}
	if !hn.store.ReadSet("2026-08-20")["f1"] {
		t.Error("unmarking cycling wiped the whole day's read state")
	}
}

func TestHomeListsCategoriesWithUnreadCounts(t *testing.T) {
	hn := setup(t, goodResponse)
	h := hn.srv.Handler()

	saveDay(t, hn.store, "2026-08-20",
		topic("c1", "cycling", "Old cycling"),
		topic("n1", "news", "Old news"))
	saveDay(t, hn.store, "2026-08-21", topic("c2", "cycling", "New cycling"))
	hn.store.SetRead("2026-08-20", "c1", true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `href="/c/cycling"`) || !strings.Contains(body, `href="/c/news"`) {
		t.Error("home does not link to both standing feeds")
	}
	// cycling: 2 topics, 1 read -> 1 unread. The count is across days.
	if !strings.Contains(body, `<span class="cat-count">1</span>`) {
		t.Errorf("home is missing the cycling unread count")
	}
	// "cycling" is not in this config at all — it only exists in stored
	// digests. Its topics must stay reachable rather than being orphaned.
	if !strings.Contains(body, `href="/c/cycling"`) {
		t.Error("a category left over from an older config was orphaned")
	}
}

// A category with a space has to survive the round trip into a URL and back.
func TestCategoryWithSpaceRoundTrips(t *testing.T) {
	hn := setup(t, goodResponse)
	h := hn.srv.Handler()

	saveDay(t, hn.store, hn.date, topic("p1", "general aviation", "Emissions target moved"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/general%20aviation", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /c/general%%20aviation = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Emissions target moved") {
		t.Error("escaped category did not resolve back to its feed")
	}
}

func TestUnknownCategoryRendersEmptyRatherThanFailing(t *testing.T) {
	hn := setup(t, goodResponse)

	rec := httptest.NewRecorder()
	hn.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/nosuchcategory", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /c/nosuchcategory = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nothing briefed into") {
		t.Error("empty category feed is missing its explanation")
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

// --- complete categories ---

// itemsFeed serves one item per title, newest first and an hour apart, so the
// index a prompt sees is deterministic.
func itemsFeed(t *testing.T, titles ...string) string {
	t.Helper()

	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Test Feed</title>`)
	for i, title := range titles {
		fmt.Fprintf(&b, `<item><title>%s</title><link>https://example.com/i%d</link>`+
			`<description>Blurb %d.</description><pubDate>%s</pubDate></item>`,
			title, i, i, time.Now().Add(-time.Duration(i)*time.Hour).UTC().Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	body := b.String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// completeSetup builds a one-category install in the given mode and runs it
// against a scripted backend.
func completeSetup(t *testing.T, mode string, script []string, titles ...string) (*store.Digest, *Server, *store.Store, string) {
	t.Helper()

	url := itemsFeed(t, titles...)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "feeds.yaml")
	cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
		"brief:\n  enabled: false\n" +
		"categories:\n  aviation:\n    mode: " + mode + "\n" +
		"feeds:\n  - name: Air Facts\n    category: aviation\n    url: " + url + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: &scriptedBackend{script: script}, Log: log}
	srv, err := New(cfg, st, gen, log)
	if err != nil {
		t.Fatal(err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	d, err := gen.Run(context.Background(), date)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return d, srv, st, date
}

// The point of a complete category: an article the model quietly failed to
// mention is added back rather than lost. This is what "I don't want to miss
// anything" has to mean in code — the guarantee cannot rest on the prompt.
func TestCompleteCategoryCoversEveryArticle(t *testing.T) {
	// The model writes up only the first of three items and excludes nothing.
	resp := `{"topics":[{"headline":"New avionics rule","summary":"It changed.","tag":"rules",
	  "importance":"high","source_indexes":[0]}],"excluded_indexes":[]}`

	d, _, _, _ := completeSetup(t, "complete", []string{resp},
		"New avionics rule", "Cessna price drop", "Grass strip reopens")

	if len(d.Topics) != 3 {
		t.Fatalf("want every article accounted for, got %d topics: %+v", len(d.Topics), d.Topics)
	}
	headlines := map[string]bool{}
	for _, tp := range d.Topics {
		headlines[tp.Headline] = true
	}
	for _, want := range []string{"New avionics rule", "Cessna price drop", "Grass strip reopens"} {
		if !headlines[want] {
			t.Errorf("article %q was dropped by a category that promised not to", want)
		}
	}

	if len(d.Categories) != 1 {
		t.Fatalf("want one category stat, got %+v", d.Categories)
	}
	stat := d.Categories[0]
	if stat.Mode != "complete" || stat.Items != 3 || stat.Covered != 3 || stat.Rescued != 2 {
		t.Errorf("coverage stat does not describe what happened: %+v", stat)
	}

	// A rescued article keeps the feed's own headline, link and blurb - nothing
	// about it is invented.
	for _, tp := range d.Topics {
		if tp.Headline != "Cessna price drop" {
			continue
		}
		if len(tp.Sources) != 1 || tp.Sources[0].URL != "https://example.com/i1" {
			t.Errorf("rescued topic lost its real source: %+v", tp.Sources)
		}
		if tp.Summary != "Blurb 1." {
			t.Errorf("rescued summary = %q, want the feed's own blurb", tp.Summary)
		}
	}
}

// Rescuing everything unmentioned would make exclusions impossible, so a
// complete category names what it drops and those items stay dropped.
func TestCompleteCategoryHonoursDeliberateExclusions(t *testing.T) {
	resp := `{"topics":[{"headline":"New avionics rule","summary":"It changed.","tag":"rules",
	  "importance":"normal","source_indexes":[0]}],"excluded_indexes":[1]}`

	d, _, _, _ := completeSetup(t, "complete", []string{resp},
		"New avionics rule", "Sponsored: buy a headset", "Grass strip reopens")

	for _, tp := range d.Topics {
		if strings.Contains(tp.Headline, "Sponsored") {
			t.Error("an item the model deliberately excluded was rescued anyway")
		}
	}
	if len(d.Topics) != 2 {
		t.Fatalf("want the excluded item gone and the lost one back, got %d", len(d.Topics))
	}
	if got := d.Categories[0]; got.Excluded != 1 || got.Rescued != 1 {
		t.Errorf("stat does not separate a deliberate drop from a lost item: %+v", got)
	}
}

// A brief category is a selection, so an item the model passed over stays
// passed over. Rescuing there would defeat the point of max_topics.
func TestBriefCategoryLeavesUnusedItemsOut(t *testing.T) {
	resp := `{"topics":[{"headline":"The one that mattered","summary":"It did.","tag":"rules",
	  "importance":"high","source_indexes":[0]}],"excluded_indexes":[]}`

	d, _, _, _ := completeSetup(t, "brief", []string{resp},
		"The one that mattered", "Minor item", "Another minor item")

	if len(d.Topics) != 1 {
		t.Fatalf("a brief must be allowed to leave things out, got %d topics", len(d.Topics))
	}
	if got := d.Categories[0]; got.Rescued != 0 || got.Covered != 1 || got.Items != 3 {
		t.Errorf("brief stat = %+v", got)
	}
}

// The two modes are told to do opposite things, and the prompt has to say so.
func TestCompleteAndBriefGetDifferentInstructions(t *testing.T) {
	for _, tc := range []struct {
		mode, want, unwanted string
	}{
		{"complete", "exactly one topic", "at most 5 topics"},
		{"brief", "at most 5 topics", "exactly one topic"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			url := itemsFeed(t, "An item")
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "feeds.yaml")
			cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
				"brief:\n  enabled: false\n" +
				"categories:\n  aviation:\n    mode: " + tc.mode + "\n" +
				"feeds:\n  - name: Air Facts\n    category: aviation\n    url: " + url + "\n"
			os.WriteFile(cfgPath, []byte(cfgYAML), 0o644)

			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			st, _ := store.New(filepath.Join(dir, "data"))
			backend := &scriptedBackend{script: []string{`{"topics":[],"excluded_indexes":[]}`}}
			gen := &digest.Generator{Cfg: cfg, Store: st, Backend: backend,
				Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if _, err := gen.Run(context.Background(), time.Now().UTC().Format("2006-01-02")); err != nil {
				t.Fatal(err)
			}

			sys := backend.reqs[0].System
			if !strings.Contains(sys, tc.want) {
				t.Errorf("%s prompt is missing %q", tc.mode, tc.want)
			}
			if strings.Contains(sys, tc.unwanted) {
				t.Errorf("%s prompt carries the other mode's rule %q", tc.mode, tc.unwanted)
			}
		})
	}
}

// A complete category reads outlet by outlet, and the outlets follow the order
// they were written down in the config.
func TestCategoryGroupedByFeed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "feeds.yaml")
	url := itemsFeed(t, "Something")
	cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
		"brief:\n  enabled: false\n" +
		"categories:\n  aviation:\n    mode: complete\n" +
		"feeds:\n" +
		"  - name: ForeFlight\n    category: aviation\n    url: " + url + "\n" +
		"  - name: Air Facts\n    category: aviation\n    url: " + url + "\n"
	os.WriteFile(cfgPath, []byte(cfgYAML), 0o644)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.New(filepath.Join(dir, "data"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: &stubBackend{}, Log: log}
	srv, err := New(cfg, st, gen, log)
	if err != nil {
		t.Fatal(err)
	}

	// Air Facts is second in the config, so it must render second even though
	// its story is listed first.
	saveDay(t, st, "2026-08-21",
		withFeed(topic("a1", "aviation", "Air Facts story"), "Air Facts"),
		withFeed(topic("f1", "aviation", "ForeFlight story"), "ForeFlight"))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/aviation", nil))
	body := rec.Body.String()

	for _, want := range []string{`<h3 class="feed-head">ForeFlight</h3>`, `<h3 class="feed-head">Air Facts</h3>`} {
		if !strings.Contains(body, want) {
			t.Errorf("by-feed grouping is missing %q", want)
		}
	}
	if strings.Index(body, "Air Facts</h3>") < strings.Index(body, "ForeFlight</h3>") {
		t.Error("outlets are not in config order")
	}
	// Every story is still there, under exactly one outlet.
	for _, want := range []string{"<h2>Air Facts story</h2>", "<h2>ForeFlight story</h2>"} {
		if n := strings.Count(body, want); n != 1 {
			t.Errorf("%q appears %d times, want once", want, n)
		}
	}
}

// A category briefed the old way keeps its flat, importance-ordered list.
func TestBriefCategoryIsNotGroupedByFeed(t *testing.T) {
	hn := setup(t, goodResponse)
	saveDay(t, hn.store, "2026-08-21", topic("n1", "news", "A news story"))

	rec := httptest.NewRecorder()
	hn.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/news", nil))
	if strings.Contains(rec.Body.String(), "feed-head") {
		t.Error("a brief category was grouped by outlet")
	}
}

// --- the front page ---

func TestFrontPageBriefRendersOnHome(t *testing.T) {
	hn := setup(t, goodResponse)

	d, _ := hn.store.LoadDigest(hn.date)
	d.Brief = []store.BriefStory{{
		Lead:     "America",
		Text:     "struck two rocket launchers near the strait.",
		TopicIDs: []string{d.Topics[0].ID},
	}, {
		Lead:     "Shimano",
		Text:     "announced a wireless groupset.",
		TopicIDs: []string{"gone-since-the-day-was-regenerated"},
	}}
	if err := hn.store.SaveDigest(d); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	hn.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "<b>America</b> struck two rocket launchers near the strait.") {
		t.Error("the lead and its sentence are not rendered as one line")
	}
	// The sources under a paragraph come from the topics it cited, so they are
	// real links from the feed rather than anything the model wrote.
	if !strings.Contains(body, "https://example.com/bridge") {
		t.Error("front page paragraph is missing the sources behind it")
	}
	// A paragraph whose topics are gone still reads; it just has no chips.
	if !strings.Contains(body, "<b>Shimano</b> announced a wireless groupset.") {
		t.Error("a paragraph citing a vanished topic was dropped instead of degrading")
	}
	// The drill-down is still there underneath.
	if !strings.Contains(body, `href="/c/news"`) {
		t.Error("front page swallowed the standing feeds")
	}
}

// A digest from before the front page existed must still render a home screen.
func TestHomeWithoutABriefStillWorks(t *testing.T) {
	hn := setup(t, goodResponse)

	rec := httptest.NewRecorder()
	hn.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Your feeds") {
		t.Error("with no brief, the feed list should still be headed")
	}
}

// The front page is written from finished topics, never from the raw feed, so
// it cannot report anything a category did not.
func TestFrontPageSeesOnlyBriefedTopics(t *testing.T) {
	h := setup(t, goodResponse)

	front := h.backend.reqs[len(h.backend.reqs)-1]
	if !strings.Contains(front.System, "front page") {
		t.Fatalf("last call was not the front page: %.120q", front.System)
	}
	if !strings.Contains(front.User, "New bridge approved") {
		t.Error("front page prompt is missing the topics it should draw on")
	}
	if strings.Contains(front.User, "Council approves the new bridge") {
		t.Error("front page was handed raw feed items rather than briefed topics")
	}
}

// Losing the front page costs a nice-to-have, not the morning's categories.
func TestFrontPageFailureKeepsTheCategories(t *testing.T) {
	url := itemsFeed(t, "A story")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "feeds.yaml")
	cfgYAML := "timezone: UTC\nrun_at: \"08:00\"\nmodel: claude-opus-5\neffort: medium\nmax_topics: 5\n" +
		"feeds:\n  - name: News Feed\n    category: news\n    url: " + url + "\n"
	os.WriteFile(cfgPath, []byte(cfgYAML), 0o644)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.New(filepath.Join(dir, "data"))
	// The category call succeeds; the front page call gets nonsense back.
	backend := &scriptedBackend{script: []string{
		`{"topics":[{"headline":"A story","summary":"x","tag":"t","importance":"normal","source_indexes":[0]}],"excluded_indexes":[]}`,
		`not json at all`,
	}}
	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: backend,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	d, err := gen.Run(context.Background(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("a failed front page took down the run: %v", err)
	}
	if len(d.Topics) != 1 {
		t.Errorf("want the category's topic to survive, got %d", len(d.Topics))
	}
	if len(d.Brief) != 0 {
		t.Errorf("want no front page, got %+v", d.Brief)
	}
	var mentioned bool
	for _, e := range d.Errors {
		if strings.Contains(e, "front page") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the front page failure was not recorded: %v", d.Errors)
	}
}

const briefResponse = `{"stories":[
  {"lead":"America","text":"struck two rocket launchers near the strait.","topic_indexes":[0]}
]}`

// The backfill exists so a day that is only missing its front page doesn't cost
// a whole re-run to fix: no feeds are fetched, no category is briefed again,
// and every topic ID — and so every read mark — stays exactly as it was.
func TestBackfillBriefWritesOnlyTheFrontPage(t *testing.T) {
	hn := setup(t, goodResponse)

	// Start from the state this is meant to repair: topics, no front page.
	before, _ := hn.store.LoadDigest(hn.date)
	before.Brief = nil
	if err := hn.store.SaveDigest(before); err != nil {
		t.Fatal(err)
	}
	if err := hn.store.SetRead(hn.date, before.Topics[0].ID, true); err != nil {
		t.Fatal(err)
	}

	backend := &scriptedBackend{script: []string{briefResponse}}
	gen := &digest.Generator{Cfg: hn.srv.Cfg, Store: hn.store, Backend: backend,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	d, err := gen.BackfillBrief(context.Background(), hn.date)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if backend.calls != 1 {
		t.Errorf("want a single front page call, got %d", backend.calls)
	}
	if len(d.Brief) != 1 || d.Brief[0].Lead != "America" {
		t.Fatalf("front page not written: %+v", d.Brief)
	}
	// It was written from the stored topics, not from the feed.
	if !strings.Contains(backend.reqs[0].User, "New bridge approved") {
		t.Error("backfill did not draw on the stored topics")
	}
	if !strings.Contains(backend.reqs[0].System, "front page") {
		t.Error("backfill made something other than a front page call")
	}

	// The categories underneath are untouched, and so is the read mark.
	if len(d.Topics) != len(before.Topics) || d.Topics[0].ID != before.Topics[0].ID {
		t.Errorf("backfill disturbed the day's topics: %+v", d.Topics)
	}
	if !hn.store.ReadSet(hn.date)[before.Topics[0].ID] {
		t.Error("backfill lost a read mark")
	}
}

// A failed attempt has to leave a trace, or startup would try again on every
// restart and pay for the call each time.
func TestBackfillRecordsAFailedAttempt(t *testing.T) {
	hn := setup(t, goodResponse)

	d, _ := hn.store.LoadDigest(hn.date)
	d.Brief = nil
	hn.store.SaveDigest(d)

	gen := &digest.Generator{Cfg: hn.srv.Cfg, Store: hn.store,
		Backend: &scriptedBackend{script: []string{"not json at all"}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}

	if _, err := gen.BackfillBrief(context.Background(), hn.date); err == nil {
		t.Fatal("a broken response came back as success")
	}

	stored, _ := hn.store.LoadDigest(hn.date)
	if !digest.BriefAttempted(stored) {
		t.Errorf("the failed attempt was not recorded: %v", stored.Errors)
	}
	if len(stored.Topics) == 0 {
		t.Error("a failed front page took the day's topics with it")
	}
}

// A call that completes but produces nothing is a result, not a gap, and has to
// be recorded as one — otherwise the day looks untried and gets retried.
func TestBackfillRecordsAnEmptyResult(t *testing.T) {
	hn := setup(t, goodResponse)

	d, _ := hn.store.LoadDigest(hn.date)
	d.Brief = nil
	hn.store.SaveDigest(d)

	gen := &digest.Generator{Cfg: hn.srv.Cfg, Store: hn.store,
		Backend: &scriptedBackend{script: []string{`{"stories":[]}`}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}

	if _, err := gen.BackfillBrief(context.Background(), hn.date); err != nil {
		t.Fatalf("an empty result is not an error: %v", err)
	}

	stored, _ := hn.store.LoadDigest(hn.date)
	if !digest.BriefAttempted(stored) {
		t.Errorf("an empty front page left the day looking untried: %v", stored.Errors)
	}
}

// A successful attempt clears the previous one's note, so Run details stops
// reporting a failure that has since been repaired.
func TestBackfillClearsAnEarlierFailure(t *testing.T) {
	hn := setup(t, goodResponse)

	d, _ := hn.store.LoadDigest(hn.date)
	d.Brief = nil
	d.Errors = []string{"road.cc: http 503", digest.BriefErrPrefix + "produced no stories"}
	hn.store.SaveDigest(d)

	gen := &digest.Generator{Cfg: hn.srv.Cfg, Store: hn.store,
		Backend: &scriptedBackend{script: []string{briefResponse}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}

	if _, err := gen.BackfillBrief(context.Background(), hn.date); err != nil {
		t.Fatal(err)
	}

	stored, _ := hn.store.LoadDigest(hn.date)
	if digest.BriefAttempted(stored) {
		t.Errorf("the repaired failure is still being reported: %v", stored.Errors)
	}
	// The unrelated feed failure is not collateral damage.
	if len(stored.Errors) != 1 || stored.Errors[0] != "road.cc: http 503" {
		t.Errorf("errors = %v, want the feed failure kept", stored.Errors)
	}
}

// Nothing to write a front page from is a no-op, not an error or a wasted call.
func TestBackfillOnAnEmptyDayDoesNothing(t *testing.T) {
	hn := setup(t, goodResponse)

	saveDay(t, hn.store, "2026-08-20") // a day that ran and found nothing

	backend := &scriptedBackend{script: []string{briefResponse}}
	gen := &digest.Generator{Cfg: hn.srv.Cfg, Store: hn.store, Backend: backend,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if _, err := gen.BackfillBrief(context.Background(), "2026-08-20"); err != nil {
		t.Fatalf("backfill on an empty day: %v", err)
	}
	if backend.calls != 0 {
		t.Errorf("want no call for a day with no topics, got %d", backend.calls)
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
