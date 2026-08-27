// Package web serves the phone-friendly digest reader.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fjaeckel/newsdigest/internal/config"
	"github.com/fjaeckel/newsdigest/internal/digest"
	"github.com/fjaeckel/newsdigest/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

const cookieName = "nd_auth"

// Server wires the store and generator up to HTTP.
type Server struct {
	Cfg   *config.Config
	Store *store.Store
	Gen   *digest.Generator
	Log   *slog.Logger

	token string
	tpl   *template.Template

	// Guards manual refreshes so a double tap can't run two generations at once.
	genMu      sync.Mutex
	generating bool
}

// New builds the server and parses templates.
func New(cfg *config.Config, st *store.Store, gen *digest.Generator, log *slog.Logger) (*Server, error) {
	funcs := template.FuncMap{
		"prettyDate": func(date string) string {
			t, err := time.Parse("2006-01-02", date)
			if err != nil {
				return date
			}
			return t.Format("Monday, 2 January 2006")
		},
		"shortDate": func(date string) string {
			t, err := time.Parse("2006-01-02", date)
			if err != nil {
				return date
			}
			return t.Format("Mon 2 Jan")
		},
		"groupSources": groupSources,
		// A category is free text from the config and can carry a space
		// ("general aviation"), so it gets escaped rather than pasted in.
		"catURL": func(cat string) string {
			return "/c/" + url.PathEscape(cat)
		},
	}

	tpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	return &Server{
		Cfg:   cfg,
		Store: st,
		Gen:   gen,
		Log:   log,
		token: os.Getenv("DIGEST_TOKEN"),
		tpl:   tpl,
	}, nil
}

// Handler returns the fully wired mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /d/{date}", s.handleDigest)
	mux.HandleFunc("GET /c/{category}", s.handleCategory)
	mux.HandleFunc("GET /archive", s.handleArchive)
	mux.HandleFunc("POST /api/read", s.handleRead)
	mux.HandleFunc("POST /api/read-all", s.handleReadAll)
	mux.HandleFunc("POST /api/read-category", s.handleReadCategory)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/status", s.handleStatus)

	return s.withAuth(mux)
}

// withAuth gates everything behind DIGEST_TOKEN when one is configured.
// A one-time ?t=<token> sets a long-lived cookie so the phone stays logged in.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" ||
			r.URL.Path == "/healthz" ||
			strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		if t := r.URL.Query().Get("t"); t != "" {
			if t != s.token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    s.token,
				Path:     "/",
				MaxAge:   int((365 * 24 * time.Hour).Seconds()),
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
			})
			// Redirect so the token stops showing up in the address bar.
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
			return
		}

		c, err := r.Cookie(cookieName)
		if err != nil || c.Value != s.token {
			http.Error(w, "forbidden - append ?t=YOUR_DIGEST_TOKEN to the URL once", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type pageData struct {
	Digest      *store.Digest
	Read        map[string]bool
	UnreadCount int
	Dates       []string
	PrevDate    string
	NextDate    string
	Generating  bool
	Title       string

	// Cards are the topics to render, in order. Both the daily digest and a
	// category feed render the same card partial, so they share one shape.
	Cards []TopicCard

	// Set on the home screen.
	Categories  []CategoryLink
	TotalUnread int
	LatestDate  string

	// Set on a category feed only.
	Category string
	Days     []FeedDay
	Total    int
	Capped   bool
}

// CategoryLink is one standing feed on the home screen.
type CategoryLink struct {
	Name   string
	Unread int
	Total  int
}

// FeedDay groups a category feed's topics under the day they were briefed.
type FeedDay struct {
	Date  string
	Cards []TopicCard
}

// TopicCard is one rendered topic. Date travels with it because a category
// feed mixes days on one page and marking read has to know which digest to
// write to.
type TopicCard struct {
	Topic store.Topic
	Read  bool
	Date  string
}

// handleHome lists the standing feeds with what is still unread in each.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	dates, err := s.Store.Dates()
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(dates) == 0 {
		s.renderEmpty(w)
		return
	}

	unread := map[string]int{}
	total := map[string]int{}
	var extra []string // categories present on disk but no longer in the config

	known := map[string]bool{}
	for _, c := range s.Cfg.Categories() {
		known[c] = true
	}

	for _, date := range dates {
		d, err := s.Store.LoadDigest(date)
		if err != nil || d == nil {
			continue
		}
		readSet := s.Store.ReadSet(date)
		for _, t := range d.Topics {
			cat := t.Category
			if cat == "" {
				cat = config.DefaultCategory
			}
			if !known[cat] && total[cat] == 0 {
				extra = append(extra, cat)
			}
			total[cat]++
			if !readSet[t.ID] {
				unread[cat]++
			}
		}
	}

	// Config order first so the reader's own ordering wins, then anything left
	// over from an older config so its topics stay reachable.
	links := make([]CategoryLink, 0, len(total))
	sum := 0
	for _, cat := range append(s.Cfg.Categories(), extra...) {
		links = append(links, CategoryLink{Name: cat, Unread: unread[cat], Total: total[cat]})
		sum += unread[cat]
	}

	s.render(w, "home.html", pageData{
		Categories:  links,
		TotalUnread: sum,
		LatestDate:  dates[0],
		Dates:       dates,
		Title:       "Digest",
	})
}

func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.LoadDigest(r.PathValue("date"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	s.renderDigest(w, d)
}

func (s *Server) renderDigest(w http.ResponseWriter, d *store.Digest) {
	readSet := s.Store.ReadSet(d.Date)

	unread := 0
	cards := make([]TopicCard, 0, len(d.Topics))
	for _, t := range d.Topics {
		if !readSet[t.ID] {
			unread++
		}
		cards = append(cards, TopicCard{Topic: t, Read: readSet[t.ID], Date: d.Date})
	}

	dates, err := s.Store.Dates()
	if err != nil {
		s.fail(w, err)
		return
	}
	// Dates are newest first, so the next-older date sits after the current one.
	var prev, next string
	for i, date := range dates {
		if date != d.Date {
			continue
		}
		if i+1 < len(dates) {
			prev = dates[i+1]
		}
		if i > 0 {
			next = dates[i-1]
		}
		break
	}

	s.genMu.Lock()
	generating := s.generating
	s.genMu.Unlock()

	s.render(w, "digest.html", pageData{
		Digest:      d,
		Read:        readSet,
		UnreadCount: unread,
		Dates:       dates,
		PrevDate:    prev,
		NextDate:    next,
		Generating:  generating,
		Title:       "Digest " + d.Date,
		Cards:       cards,
	})
}

// maxFeedTopics bounds how much of a category feed is rendered at once. The
// archive is pruned, but a long-lived install shouldn't build an unbounded
// page on a phone. Unread counts are tallied over everything regardless, so
// the header stays honest even when the list below it is trimmed.
const maxFeedTopics = 200

// handleCategory serves one standing feed: every topic briefed into that
// category, newest day first, carried forward until it is read.
func (s *Server) handleCategory(w http.ResponseWriter, r *http.Request) {
	cat := strings.ToLower(strings.TrimSpace(r.PathValue("category")))
	if cat == "" {
		http.NotFound(w, r)
		return
	}

	dates, err := s.Store.Dates()
	if err != nil {
		s.fail(w, err)
		return
	}

	var (
		days     []FeedDay
		rendered int
		total    int
		unread   int
		capped   bool
	)
	for _, date := range dates {
		d, err := s.Store.LoadDigest(date)
		if err != nil || d == nil {
			// A single unreadable day shouldn't take the whole feed down.
			continue
		}
		readSet := s.Store.ReadSet(date)

		var cards []TopicCard
		for _, t := range d.Topics {
			if topicCategory(t) != cat {
				continue
			}
			total++
			if !readSet[t.ID] {
				unread++
			}
			if rendered >= maxFeedTopics {
				capped = true
				continue
			}
			cards = append(cards, TopicCard{Topic: t, Read: readSet[t.ID], Date: date})
			rendered++
		}
		if len(cards) > 0 {
			days = append(days, FeedDay{Date: date, Cards: cards})
		}
	}

	s.render(w, "category.html", pageData{
		Dates:       dates,
		Category:    cat,
		Days:        days,
		Total:       total,
		UnreadCount: unread,
		Capped:      capped,
		Title:       cat,
	})
}

// topicCategory falls back for digests written before categories existed.
func topicCategory(t store.Topic) string {
	if t.Category == "" {
		return config.DefaultCategory
	}
	return t.Category
}

func (s *Server) renderEmpty(w http.ResponseWriter) {
	s.genMu.Lock()
	generating := s.generating
	s.genMu.Unlock()
	s.render(w, "empty.html", pageData{Generating: generating, Title: "News digest"})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	dates, err := s.Store.Dates()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "archive.html", pageData{Dates: dates, Title: "Archive"})
}

// --- API ---

type readRequest struct {
	Date string `json:"date"`
	ID   string `json:"id"`
	Read bool   `json:"read"`
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req readRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Date == "" || req.ID == "" {
		http.Error(w, "date and id are required", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetRead(req.Date, req.ID, req.Read); err != nil {
		s.fail(w, err)
		return
	}
	s.writeUnread(w, req.Date)
}

func (s *Server) handleReadAll(w http.ResponseWriter, r *http.Request) {
	var req readRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d, err := s.Store.LoadDigest(req.Date)
	if err != nil {
		s.fail(w, err)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	ids := make([]string, 0, len(d.Topics))
	for _, t := range d.Topics {
		ids = append(ids, t.ID)
	}
	if err := s.Store.SetAllRead(req.Date, ids, req.Read); err != nil {
		s.fail(w, err)
		return
	}
	s.writeUnread(w, req.Date)
}

type readCategoryRequest struct {
	Category string `json:"category"`
	Read     bool   `json:"read"`
}

// handleReadCategory clears (or restores) a whole standing feed. Because the
// feed carries unread items forward indefinitely, there has to be a way to
// draw a line under it without opening every day it spans.
func (s *Server) handleReadCategory(w http.ResponseWriter, r *http.Request) {
	var req readCategoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cat := strings.ToLower(strings.TrimSpace(req.Category))
	if cat == "" {
		http.Error(w, "category is required", http.StatusBadRequest)
		return
	}

	dates, err := s.Store.Dates()
	if err != nil {
		s.fail(w, err)
		return
	}

	for _, date := range dates {
		d, err := s.Store.LoadDigest(date)
		if err != nil || d == nil {
			continue
		}
		var ids []string
		for _, t := range d.Topics {
			if topicCategory(t) == cat {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		// A subset of the day: other categories briefed on this date keep
		// their own read marks.
		if err := s.Store.SetManyRead(date, ids, req.Read); err != nil {
			s.fail(w, err)
			return
		}
	}

	// Report what is still unread in this category, for the header.
	unread := 0
	for _, date := range dates {
		d, err := s.Store.LoadDigest(date)
		if err != nil || d == nil {
			continue
		}
		readSet := s.Store.ReadSet(date)
		for _, t := range d.Topics {
			if topicCategory(t) == cat && !readSet[t.ID] {
				unread++
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true, "unread": unread})
}

func (s *Server) writeUnread(w http.ResponseWriter, date string) {
	d, err := s.Store.LoadDigest(date)
	if err != nil || d == nil {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	readSet := s.Store.ReadSet(date)
	unread := 0
	for _, t := range d.Topics {
		if !readSet[t.ID] {
			unread++
		}
	}
	writeJSON(w, map[string]any{"ok": true, "unread": unread})
}

// handleRefresh regenerates today's digest on demand. It returns immediately;
// the page polls until the run finishes.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.genMu.Lock()
	if s.generating {
		s.genMu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "already_running": true})
		return
	}
	s.generating = true
	s.genMu.Unlock()

	date := time.Now().In(s.Cfg.Location).Format("2006-01-02")

	go func() {
		defer func() {
			s.genMu.Lock()
			s.generating = false
			s.genMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		if _, err := s.Gen.Run(ctx, date); err != nil {
			s.Log.Error("manual refresh failed", "err", err)
		}
	}()

	writeJSON(w, map[string]any{"ok": true, "started": true})
}

// handleStatus lets the page poll a generation it kicked off, without having to
// scrape the rendered HTML for a spinner.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.genMu.Lock()
	generating := s.generating
	s.genMu.Unlock()

	latest := ""
	if d, err := s.Store.Latest(); err == nil && d != nil {
		latest = d.Date
	}
	writeJSON(w, map[string]any{"generating": generating, "latest": latest})
}

// --- helpers ---

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error("render failed", "template", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// GroupedSource gathers the articles one outlet ran on a story. A big story
// pulls several pieces from the same paper and they're all legitimately
// different articles, so nothing is discarded: the outlet renders as a single
// chip that expands to the individual pieces when it holds more than one.
type GroupedSource struct {
	Feed     string
	Articles []store.Source
}

func groupSources(sources []store.Source) []GroupedSource {
	var out []GroupedSource
	seen := map[string]int{} // feed -> index in out, preserving first-seen order

	for _, s := range sources {
		if i, ok := seen[s.Feed]; ok {
			out[i].Articles = append(out[i].Articles, s)
			continue
		}
		seen[s.Feed] = len(out)
		out = append(out, GroupedSource{Feed: s.Feed, Articles: []store.Source{s}})
	}
	return out
}
