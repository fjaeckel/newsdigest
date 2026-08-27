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

	// If the container starts after today's scheduled time and nothing has been
	// generated yet, catch up instead of showing an empty page until tomorrow.
	go catchUp(context.Background(), log, cfg, st, gen)

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

func catchUp(ctx context.Context, log *slog.Logger, cfg *config.Config, st *store.Store, gen *digest.Generator) {
	now := time.Now().In(cfg.Location)
	today := now.Format("2006-01-02")

	if now.Before(cfg.RunAtToday(now)) {
		return // today's run hasn't come round yet
	}
	existing, err := st.LoadDigest(today)
	if err != nil {
		log.Warn("catch-up check failed", "err", err)
		return
	}
	if existing != nil {
		return
	}

	log.Info("no digest for today and the scheduled time has passed; generating now", "date", today)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if _, err := gen.Run(ctx, today); err != nil {
		log.Error("catch-up digest failed", "err", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
