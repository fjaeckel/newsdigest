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

	mux.HandleFunc("GET /{$}", s.handleLatest)
	mux.HandleFunc("GET /d/{date}", s.handleDigest)
	mux.HandleFunc("GET /archive", s.handleArchive)
	mux.HandleFunc("POST /api/read", s.handleRead)
	mux.HandleFunc("POST /api/read-all", s.handleReadAll)
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
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.Latest()
	if err != nil {
		s.fail(w, err)
		return
	}
	if d == nil {
		s.renderEmpty(w)
		return
	}
	s.renderDigest(w, d)
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
	for _, t := range d.Topics {
		if !readSet[t.ID] {
			unread++
		}
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
	})
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
