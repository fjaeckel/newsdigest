// Command newsdigest fetches your RSS feeds once a day, has Claude turn them
// into a short brief, and serves that brief as a phone-friendly web page.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/fjaeckel/newsdigest/internal/claude"
	"github.com/fjaeckel/newsdigest/internal/config"
	"github.com/fjaeckel/newsdigest/internal/digest"
	"github.com/fjaeckel/newsdigest/internal/store"
	"github.com/fjaeckel/newsdigest/internal/web"
)

func main() {
	var (
		once = flag.Bool("once", false, "generate today's digest, print nothing more, and exit")
		date = flag.String("date", "", "date to generate for with -once (YYYY-MM-DD, default today)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log, *once, *date); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, once bool, dateFlag string) error {
	cfg, err := config.Load(env("NEWSDIGEST_CONFIG", "config/feeds.yaml"))
	if err != nil {
		return err
	}

	st, err := store.New(env("NEWSDIGEST_DATA", "data"))
	if err != nil {
		return err
	}

	backend, err := claude.New()
	if err != nil {
		return err
	}
	log.Info("configured",
		"backend", backend.Name(),
		"model", cfg.Model,
		"effort", cfg.Effort,
		"feeds", len(cfg.Feeds),
		"run_at", cfg.RunAt,
		"timezone", cfg.Timezone)

	gen := &digest.Generator{Cfg: cfg, Store: st, Backend: backend, Log: log}

	if once {
		d := dateFlag
		if d == "" {
			d = time.Now().In(cfg.Location).Format("2006-01-02")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		_, err := gen.Run(ctx, d)
		return err
	}

	srv, err := web.New(cfg, st, gen, log)
	if err != nil {
		return err
	}

	keepDays, _ := strconv.Atoi(env("NEWSDIGEST_KEEP_DAYS", "30"))

	// --- scheduler ---

	c := cron.New(cron.WithLocation(cfg.Location))
	if _, err := c.AddFunc(cfg.CronSpec, func() {
		today := time.Now().In(cfg.Location).Format("2006-01-02")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		if _, err := gen.Run(ctx, today); err != nil {
			log.Error("scheduled digest failed", "date", today, "err", err)
			return
		}
		if err := st.Prune(keepDays, time.Now().In(cfg.Location)); err != nil {
			log.Warn("prune failed", "err", err)
		}
	}); err != nil {
		return err
	}
	c.Start()
	defer c.Stop()

	if entries := c.Entries(); len(entries) > 0 {
		log.Info("next scheduled run", "at", entries[0].Next.Format(time.RFC1123))
	}

	// Make sure today has a brief before anyone opens the page. Runs in the
	// background so the server starts listening immediately.
	go ensureToday(context.Background(), log, cfg, st, gen)

	// --- http ---

	addr := env("NEWSDIGEST_ADDR", ":8080")
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "auth", os.Getenv("DIGEST_TOKEN") != "")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}

// ensureToday gives the reader something to open on startup.
//
// Nothing synced at all today — no run has happened — is the case that ignores
// the clock: waiting for run_at would leave an empty page for hours, and there
// is nothing to lose by briefing early.
//
// A day that ran and produced nothing in any category is different. That is a
// real, completed run over a quiet lookback window, so the run_at guard still
// applies to retrying it: before the scheduled time a restart leaves it alone,
// after it a retry is worth the tokens because the feeds have had all morning
// to publish.
//
// A day that already has topics is never regenerated here, however uneven the
// categories are. A category is legitimately empty on plenty of days, so
// treating a quiet one as a gap to fill would re-run the model on every restart.
func ensureToday(ctx context.Context, log *slog.Logger, cfg *config.Config, st *store.Store, gen *digest.Generator) {
	now := time.Now().In(cfg.Location)
	today := now.Format("2006-01-02")

	existing, err := st.LoadDigest(today)
	if err != nil {
		log.Warn("startup digest check failed", "err", err)
		return
	}

	runAt := cfg.RunAtToday(now)

	generate, reason := shouldBriefOnStartup(existing, now, runAt)
	if generate {
		log.Info("startup: briefing today", "date", today, "reason", reason)

		ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		if _, err := gen.Run(ctx, today); err != nil {
			log.Error("startup digest failed", "date", today, "err", err)
		}
		return
	}
	log.Info("startup: leaving today alone", "date", today, "reason", reason)

	// A full run isn't warranted, but the day can still be missing its front
	// page — briefed before the front page existed, or briefed on a morning
	// when that one call failed. Writing it needs no feeds and no category
	// briefs, so it is worth doing on its own rather than waiting for tomorrow.
	backfill, reason := shouldBackfillBrief(existing, cfg.Brief.On(), now, runAt)
	if !backfill {
		log.Info("startup: not writing a front page", "date", today, "reason", reason)
		return
	}
	log.Info("startup: writing today's front page on its own", "date", today, "reason", reason)

	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if _, err := gen.BackfillBrief(ctx, today); err != nil {
		log.Error("startup front page failed", "date", today, "err", err)
	}
}

// shouldBackfillBrief decides whether today needs its front page written on its
// own. It only ever fires for a day that already has topics, so it can never be
// the thing that briefs a morning — at worst it adds a page that was missing.
//
// The clock matters in exactly one case. A day that has simply never been asked
// for a front page has no failure to retry, so it is written immediately. A day
// whose front page call already ran and came back with nothing is a completed
// attempt, and retrying that on every restart would spend a call each time, so
// it waits for run_at the way an empty morning does.
func shouldBackfillBrief(existing *store.Digest, briefOn bool, now, runAt time.Time) (bool, string) {
	// A day that is about to be rebuilt in full gets its front page out of that
	// run, so backfilling as well would buy the same page twice. The caller
	// already returns before reaching here, but the two decisions being
	// genuinely disjoint is a property of the decisions, not of the order two
	// statements happen to sit in.
	if full, _ := shouldBriefOnStartup(existing, now, runAt); full {
		return false, "a full brief is running instead"
	}

	switch {
	case !briefOn:
		return false, "front page switched off in config"
	case existing == nil || len(existing.Topics) == 0:
		return false, "no topics to write a front page from"
	case len(existing.Brief) > 0:
		return false, "already has a front page"
	case !digest.BriefAttempted(existing):
		return true, "day has no front page yet"
	case now.Before(runAt):
		return false, "front page already failed today, before run_at"
	default:
		return true, "front page already failed today, run_at has passed"
	}
}

// shouldBriefOnStartup is the decision above, split out so it can be tested
// without a store, a backend or a clock.
func shouldBriefOnStartup(existing *store.Digest, now, runAt time.Time) (bool, string) {
	switch {
	case existing == nil:
		return true, "nothing synced today"
	case len(existing.Topics) > 0 && !anyCategorised(existing.Topics):
		// Written before feeds had categories, so every topic would land in
		// "general" and the standing feeds would all read empty. Re-brief it
		// once, whatever the clock says; config.Load gives every feed a
		// category, so a fresh run can never look like this again.
		return true, "today predates categories"
	case len(existing.Topics) > 0:
		return false, "already briefed"
	case now.Before(runAt):
		return false, "ran empty, before run_at"
	default:
		return true, "ran empty, run_at has passed"
	}
}

func anyCategorised(topics []store.Topic) bool {
	for _, t := range topics {
		if t.Category != "" {
			return true
		}
	}
	return false
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
