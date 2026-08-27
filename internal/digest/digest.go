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

	// Each category is briefed on its own, so max_topics is a per-category
	// budget and a quiet category can't be squeezed out by a loud one. One
	// category failing costs that category its brief, not the whole morning.
	for _, cat := range g.Cfg.Categories() {
		items := itemsInCategory(fetched.Items, cat)
		if len(items) == 0 {
			continue
		}

		topics, err := g.briefCategory(ctx, cat, items, date)
		if err != nil {
			g.Log.Error("category brief failed", "category", cat, "err", err)
			d.Errors = append(d.Errors, fmt.Sprintf("%s: %v", cat, err))
			continue
		}
		g.Log.Info("category briefed", "category", cat, "items", len(items), "topics", len(topics))
		d.Topics = append(d.Topics, topics...)
	}

	if len(d.Topics) == 0 && len(d.Errors) > len(fetched.Errors) {
		return nil, fmt.Errorf("every category failed to summarise")
	}

	d.Stats.Topics = len(d.Topics)
	d.Stats.DurationSecs = int(time.Since(started).Seconds())

	if err := g.Store.SaveDigest(d); err != nil {
		return nil, err
	}
	g.Log.Info("digest saved", "date", date, "topics", len(d.Topics), "took", time.Since(started).Round(time.Second))
	return d, nil
}

// briefCategory runs one category's items through Claude and returns its topics.
func (g *Generator) briefCategory(ctx context.Context, cat string, items []feeds.Item, date string) ([]store.Topic, error) {
	raw, err := g.Backend.Complete(ctx, claude.Request{
		System: systemPrompt(g.Cfg, cat),
		User:   userPrompt(items, g.Cfg.Location),
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

	return buildTopics(parsed, items, date, cat), nil
}

// itemsInCategory keeps the newest-first order the fetcher produced.
func itemsInCategory(items []feeds.Item, cat string) []feeds.Item {
	var out []feeds.Item
	for _, it := range items {
		if it.Category == cat {
			out = append(out, it)
		}
	}
	return out
}

// buildTopics maps the model's source indexes back onto the real items, so
// links and outlet names come from the feed rather than from the model.
func buildTopics(parsed modelTopics, items []feeds.Item, date, cat string) []store.Topic {
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
			ID:        topicID(date, cat, headline),
			Category:  cat,
			Headline:  headline,
			Summary:   strings.TrimSpace(t.Summary),
			Tag:       strings.ToLower(strings.TrimSpace(t.Tag)),
			Important: t.Importance == "high",
			Sources:   sources,
		})
	}
	return topics
}

// topicID is stable for a given date+category+headline so read marks survive a
// rerun of the same day.
func topicID(date, cat, headline string) string {
	sum := sha1.Sum([]byte(date + "\x00" + cat + "\x00" + strings.ToLower(headline)))
	return hex.EncodeToString(sum[:])[:12]
}

func systemPrompt(cfg *config.Config, cat string) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are the editor of one person's daily briefing on a single subject: %s. You receive raw items pulled from the feeds they follow on that subject over the last day, and turn them into a short brief they can skim over coffee.

Every item you are given belongs to this subject already, so do not set anything aside for being off-topic, niche, or too specialist. Judge importance within %s, not against the news of the world: for a %s brief, the biggest %s story of the day leads, even on a day when it would never make a general front page.

Rules:`, cat, cat, cat, cat)
	b.WriteString(`
- Group items covering the same story into ONE topic, even across different outlets. Merge aggressively.
- Order topics by how much they matter to someone who follows this subject closely. Most important first.
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
