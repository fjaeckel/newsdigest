// Package config loads the YAML configuration that drives the digest.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Feed is a single RSS/Atom source.
type Feed struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// Exclude describes what never makes it into the brief. Topics are handed to
// Claude verbatim; Keywords are a cheap local pre-filter applied first.
type Exclude struct {
	Topics   []string `yaml:"topics"`
	Keywords []string `yaml:"keywords"`
}

// Config is the parsed feeds.yaml plus a few derived fields.
type Config struct {
	Timezone        string  `yaml:"timezone"`
	RunAt           string  `yaml:"run_at"`
	LookbackHours   int     `yaml:"lookback_hours"`
	MaxItemsPerFeed int     `yaml:"max_items_per_feed"`
	MaxItemsTotal   int     `yaml:"max_items_total"`
	MaxTopics       int     `yaml:"max_topics"`
	Language        string  `yaml:"language"`
	Model           string  `yaml:"model"`
	Effort          string  `yaml:"effort"`
	Exclude         Exclude `yaml:"exclude"`
	Feeds           []Feed  `yaml:"feeds"`

	// Derived, not read from YAML.
	Location *time.Location `yaml:"-"`
	CronSpec string         `yaml:"-"`
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Timezone:        "UTC",
		RunAt:           "08:00",
		LookbackHours:   26,
		MaxItemsPerFeed: 40,
		MaxItemsTotal:   250,
		MaxTopics:       12,
		Language:        "English",
		Model:           "claude-opus-5",
		Effort:          "medium",
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if len(cfg.Feeds) == 0 {
		return nil, fmt.Errorf("config has no feeds")
	}
	for i, f := range cfg.Feeds {
		if strings.TrimSpace(f.URL) == "" {
			return nil, fmt.Errorf("feed %d has no url", i+1)
		}
		if strings.TrimSpace(f.Name) == "" {
			cfg.Feeds[i].Name = f.URL
		}
	}

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", cfg.Timezone, err)
	}
	cfg.Location = loc

	cfg.CronSpec, err = cronSpec(cfg.RunAt)
	if err != nil {
		return nil, err
	}

	switch cfg.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return nil, fmt.Errorf("effort must be one of low/medium/high/xhigh/max, got %q", cfg.Effort)
	}

	// Lower-case the keyword list once so matching is a plain comparison later.
	for i, k := range cfg.Exclude.Keywords {
		cfg.Exclude.Keywords[i] = strings.ToLower(strings.TrimSpace(k))
	}

	return cfg, nil
}

// cronSpec turns "08:00" into a robfig/cron daily spec.
func cronSpec(runAt string) (string, error) {
	h, m, ok := strings.Cut(runAt, ":")
	if !ok {
		return "", fmt.Errorf("run_at must look like HH:MM, got %q", runAt)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hour < 0 || hour > 23 {
		return "", fmt.Errorf("run_at hour out of range in %q", runAt)
	}
	minute, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil || minute < 0 || minute > 59 {
		return "", fmt.Errorf("run_at minute out of range in %q", runAt)
	}
	return fmt.Sprintf("%d %d * * *", minute, hour), nil
}

// RunAtToday returns today's scheduled generation time in the configured zone.
func (c *Config) RunAtToday(now time.Time) time.Time {
	local := now.In(c.Location)
	var hour, minute int
	fmt.Sscanf(c.RunAt, "%d:%d", &hour, &minute)
	return time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, c.Location)
}
