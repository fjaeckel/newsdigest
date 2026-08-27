// Package digest turns a morning's feed items into a stored brief.
package digest

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fjaeckel/newsdigest/internal/claude"
	"github.com/fjaeckel/newsdigest/internal/config"
	"github.com/fjaeckel/newsdigest/internal/feeds"
	"github.com/fjaeckel/newsdigest/internal/store"
)

// Generator builds digests and hands them to the store.
type Generator struct {
	Cfg     *config.Config
	Store   *store.Store
	Backend claude.Backend
	Log     *slog.Logger
}

// modelTopics is the shape Claude is asked to return.
type modelTopics struct {
	Topics []struct {
		Headline      string `json:"headline"`
		Summary       string `json:"summary"`
		Tag           string `json:"tag"`
		Importance    string `json:"importance"`
		SourceIndexes []int  `json:"source_indexes"`
	} `json:"topics"`
}

func schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"topics": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"headline":   map[string]any{"type": "string"},
						"summary":    map[string]any{"type": "string"},
						"tag":        map[string]any{"type": "string"},
						"importance": map[string]any{"type": "string", "enum": []string{"high", "normal"}},
						"source_indexes": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "integer"},
						},
					},
					"required":             []string{"headline", "summary", "tag", "importance", "source_indexes"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"topics"},
		"additionalProperties": false,
	}
}

// Run generates the digest for the given date and saves it.
func (g *Generator) Run(ctx context.Context, date string) (*store.Digest, error) {
	started := time.Now()
	g.Log.Info("generating digest", "date", date, "feeds", len(g.Cfg.Feeds))

	fetched := feeds.Fetch(ctx, g.Cfg)
	for _, e := range fetched.Errors {
		g.Log.Warn("feed failed", "detail", e)
	}
	g.Log.Info("fetched items",
		"items", len(fetched.Items),
		"prefiltered", fetched.Excluded,
		"feeds_ok", fetched.FeedsOK,
		"feeds_failed", len(fetched.Errors))

	d := &store.Digest{
		Date:        date,
		GeneratedAt: time.Now().In(g.Cfg.Location),
		Model:       g.Cfg.Model,
		Backend:     g.Backend.Name(),
		Errors:      fetched.Errors,
		Stats: store.Stats{
			Feeds:       fetched.FeedsOK,
			FeedsFailed: len(fetched.Errors),
			Items:       len(fetched.Items),
			FilteredOut: fetched.Excluded,
		},
		Topics: []store.Topic{},
	}

	if len(fetched.Items) == 0 {
		d.Stats.DurationSecs = int(time.Since(started).Seconds())
		if err := g.Store.SaveDigest(d); err != nil {
			return nil, err
		}
		g.Log.Warn("no items in lookback window; saved empty digest", "date", date)
		return d, nil
	}

	raw, err := g.Backend.Complete(ctx, claude.Request{
		System: systemPrompt(g.Cfg),
		User:   userPrompt(fetched.Items, g.Cfg.Location),
		Model:  g.Cfg.Model,
		Effort: g.Cfg.Effort,
		Schema: schema(),
	})
	if err != nil {
		return nil, fmt.Errorf("summarise: %w", err)
	}

	jsonText, err := claude.ExtractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w (got %.200q)", err, raw)
	}
	var parsed modelTopics
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w (got %.200q)", err, jsonText)
	}

	d.Topics = buildTopics(parsed, fetched.Items, date)
	d.Stats.Topics = len(d.Topics)
	d.Stats.DurationSecs = int(time.Since(started).Seconds())

	if err := g.Store.SaveDigest(d); err != nil {
		return nil, err
	}
	g.Log.Info("digest saved", "date", date, "topics", len(d.Topics), "took", time.Since(started).Round(time.Second))
	return d, nil
}

// buildTopics maps the model's source indexes back onto the real items, so
// links and outlet names come from the feed rather than from the model.
func buildTopics(parsed modelTopics, items []feeds.Item, date string) []store.Topic {
	topics := make([]store.Topic, 0, len(parsed.Topics))

	for _, t := range parsed.Topics {
		headline := strings.TrimSpace(t.Headline)
		if headline == "" {
			continue
		}

		seen := map[string]bool{}
		sources := make([]store.Source, 0, len(t.SourceIndexes))
		for _, idx := range t.SourceIndexes {
			if idx < 0 || idx >= len(items) {
				continue // hallucinated index; drop it rather than fabricate a link
			}
			it := items[idx]
			if it.Link == "" || seen[it.Link] {
				continue
			}
			seen[it.Link] = true
			sources = append(sources, store.Source{Title: it.Title, URL: it.Link, Feed: it.Feed})
		}

		topics = append(topics, store.Topic{
			ID:        topicID(date, headline),
			Headline:  headline,
			Summary:   strings.TrimSpace(t.Summary),
			Tag:       strings.ToLower(strings.TrimSpace(t.Tag)),
			Important: t.Importance == "high",
			Sources:   sources,
		})
	}
	return topics
}

// topicID is stable for a given date+headline so read marks survive a rerun.
func topicID(date, headline string) string {
	sum := sha1.Sum([]byte(date + "\x00" + strings.ToLower(headline)))
	return hex.EncodeToString(sum[:])[:12]
}

func systemPrompt(cfg *config.Config) string {
	var b strings.Builder

	b.WriteString(`You are the editor of one person's daily news briefing. You receive raw items pulled from their RSS feeds over the last day and turn them into a short brief they can skim over coffee in about two minutes.

Rules:
- Group items covering the same story into ONE topic, even across different outlets. Merge aggressively.
- Order topics by how much they matter to a generally informed reader. Most important first.
- headline: at most 80 characters, plain and factual. No clickbait, no "here's why", no questions.
- summary: 1-3 sentences, 45 words maximum. What happened, and why it matters. Assume the reader has not followed the story, but do not pad with background.
- Never invent facts, numbers, or quotes. Many items give you only a headline; when that is all you have, keep the summary to what it supports.
- tag: one lower-case word or two, e.g. "world", "tech", "germany", "business", "science", "climate".
- importance: "high" for the handful of topics worth reading even on a busy morning, "normal" for the rest.
- source_indexes: the index of every item you used for that topic, from the numbered list.
`)

	fmt.Fprintf(&b, "- Produce at most %d topics. If there is more news than that, drop the least important rather than shortening everything.\n", cfg.MaxTopics)
	fmt.Fprintf(&b, "- Write the brief in %s, regardless of the language of the source items.\n", cfg.Language)

	if len(cfg.Exclude.Topics) > 0 {
		b.WriteString("\nDrop entirely - do not summarise, do not mention, do not fold into another topic:\n")
		for _, t := range cfg.Exclude.Topics {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(t))
		}
	}
	return b.String()
}

func userPrompt(items []feeds.Item, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here are %d items from my feeds. Build today's brief.\n\n", len(items))

	for i, it := range items {
		fmt.Fprintf(&b, "[%d] %s (%s, %s)\n", i, it.Title, it.Feed, it.Published.In(loc).Format("Mon 15:04"))
		if it.Summary != "" {
			fmt.Fprintf(&b, "    %s\n", it.Summary)
		}
	}
	return b.String()
}
