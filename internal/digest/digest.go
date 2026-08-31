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

// completeChunk bounds how many items one call of a complete category has to
// account for. A category that must cover everything can't answer a long day by
// dropping the tail, so a long day is split across several calls instead.
// Items arrive newest first, so two outlets covering the same thing land close
// together and usually inside the same chunk — which is where merging can still
// see them.
const completeChunk = 60

// modelTopics is the shape Claude is asked to return.
type modelTopics struct {
	Topics []struct {
		Headline      string `json:"headline"`
		Summary       string `json:"summary"`
		Tag           string `json:"tag"`
		Importance    string `json:"importance"`
		SourceIndexes []int  `json:"source_indexes"`
	} `json:"topics"`
	// ExcludedIndexes is how a complete category says "I left this out on
	// purpose". Without it there is no way to tell a deliberate drop under an
	// exclude rule from an item the model simply lost, and the two have to be
	// treated very differently.
	ExcludedIndexes []int `json:"excluded_indexes"`
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
			"excluded_indexes": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "integer"},
			},
		},
		"required":             []string{"topics", "excluded_indexes"},
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
	for _, cat := range g.Cfg.CategoryNames() {
		items := itemsInCategory(fetched.Items, cat)
		if len(items) == 0 {
			continue
		}

		topics, stat, err := g.briefCategory(ctx, cat, items, date)
		if err != nil {
			g.Log.Error("category brief failed", "category", cat, "err", err)
			d.Errors = append(d.Errors, fmt.Sprintf("%s: %v", cat, err))
			continue
		}
		g.Log.Info("category briefed",
			"category", cat, "mode", stat.Mode, "items", stat.Items,
			"topics", stat.Topics, "covered", stat.Covered, "rescued", stat.Rescued)
		d.Topics = append(d.Topics, topics...)
		d.Categories = append(d.Categories, stat)
	}

	if len(d.Topics) == 0 && len(d.Errors) > len(fetched.Errors) {
		return nil, fmt.Errorf("every category failed to summarise")
	}

	// The front page is written from the finished topics rather than from the
	// raw feed, so it can only ever say things a category already said, and
	// every paragraph can point at the real articles underneath it.
	if g.Cfg.Brief.On() && len(d.Topics) > 0 {
		brief, err := g.frontBrief(ctx, d.Topics)
		if err != nil {
			// The categories are the substance; losing the front page costs a
			// nice-to-have, not the morning.
			g.Log.Error("front page brief failed", "err", err)
		} else {
			g.Log.Info("front page briefed", "stories", len(brief))
		}
		noteBrief(d, brief, err)
	}

	d.Stats.Topics = len(d.Topics)
	d.Stats.DurationSecs = int(time.Since(started).Seconds())

	if err := g.Store.SaveDigest(d); err != nil {
		return nil, err
	}
	g.Log.Info("digest saved", "date", date, "topics", len(d.Topics), "took", time.Since(started).Round(time.Second))
	return d, nil
}

// briefCategory runs one category's items through Claude and returns its
// topics along with the audit trail of what happened to each item.
func (g *Generator) briefCategory(ctx context.Context, cat string, items []feeds.Item, date string) ([]store.Topic, store.CategoryStat, error) {
	settings := g.Cfg.CategoryOf(cat)
	stat := store.CategoryStat{Name: cat, Mode: settings.Mode, Items: len(items)}

	// A brief category is a selection, so it is always one call over the whole
	// day: the model can only rank items it has seen together. A complete
	// category is a list, and a list splits cleanly.
	chunks := [][]feeds.Item{items}
	if settings.Mode == config.ModeComplete {
		chunks = chunkItems(items, completeChunk)
	}

	var topics []store.Topic
	for i, chunk := range chunks {
		parsed, err := g.callBrief(ctx, cat, settings, chunk, len(chunks) > 1, i, len(chunks))
		if err != nil {
			return nil, stat, err
		}

		topics = append(topics, buildTopics(parsed, chunk, date, cat)...)

		// Chunks are disjoint slices of the same list, so per-chunk tallies add
		// up to the category's without any risk of double counting.
		used := usedIndexes(parsed, len(chunk))
		dropped := indexSet(parsed.ExcludedIndexes, len(chunk))
		stat.Covered += len(used)
		stat.Excluded += len(dropped)

		// The completeness promise is kept here, in code, not in the prompt: an
		// item the model neither used nor deliberately excluded is added back as
		// its own topic. Unsummarised is a much better failure than absent.
		if settings.Mode == config.ModeComplete {
			for idx, it := range chunk {
				if used[idx] || dropped[idx] {
					continue
				}
				topics = append(topics, rescueTopic(it, date, cat))
				stat.Covered++
				stat.Rescued++
			}
		}
	}

	stat.Topics = len(topics)
	return topics, stat, nil
}

// callBrief is one Claude call: prompt, parse, hand back the raw shape.
func (g *Generator) callBrief(ctx context.Context, cat string, settings config.Category, items []feeds.Item, chunked bool, chunk, chunks int) (modelTopics, error) {
	var parsed modelTopics

	raw, err := g.Backend.Complete(ctx, claude.Request{
		System: systemPrompt(g.Cfg, cat, settings, chunked, chunk, chunks),
		User:   userPrompt(items, g.Cfg.Location, settings.Mode),
		Model:  g.Cfg.Model,
		Effort: g.Cfg.Effort,
		Schema: schema(),
	})
	if err != nil {
		return parsed, fmt.Errorf("summarise: %w", err)
	}

	jsonText, err := claude.ExtractJSON(raw)
	if err != nil {
		return parsed, fmt.Errorf("parse response: %w (got %.200q)", err, raw)
	}
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return parsed, fmt.Errorf("decode response: %w (got %.200q)", err, jsonText)
	}
	return parsed, nil
}

// chunkItems splits items into runs of at most n, preserving order.
func chunkItems(items []feeds.Item, n int) [][]feeds.Item {
	var out [][]feeds.Item
	for i := 0; i < len(items); i += n {
		end := min(i+n, len(items))
		out = append(out, items[i:end])
	}
	return out
}

// usedIndexes is every in-range item index the model referenced from a topic.
func usedIndexes(parsed modelTopics, n int) map[int]bool {
	used := map[int]bool{}
	for _, t := range parsed.Topics {
		if strings.TrimSpace(t.Headline) == "" {
			continue // buildTopics drops these, so their sources aren't covered
		}
		for _, idx := range t.SourceIndexes {
			if idx >= 0 && idx < n {
				used[idx] = true
			}
		}
	}
	return used
}

func indexSet(idxs []int, n int) map[int]bool {
	out := map[int]bool{}
	for _, idx := range idxs {
		if idx >= 0 && idx < n {
			out[idx] = true
		}
	}
	return out
}

// rescueTopic turns an item the model lost into a topic of its own, carrying
// the feed's own headline and blurb rather than anything invented.
func rescueTopic(it feeds.Item, date, cat string) store.Topic {
	return store.Topic{
		ID:       topicID(date, cat, it.Title),
		Category: cat,
		Headline: it.Title,
		Summary:  truncateWords(it.Summary, 45),
		Sources:  []store.Source{{Title: it.Title, URL: it.Link, Feed: it.Feed}},
	}
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

func truncateWords(s string, max int) string {
	words := strings.Fields(s)
	if len(words) <= max {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:max], " ") + "…"
}

// --- category prompts ---

func systemPrompt(cfg *config.Config, cat string, settings config.Category, chunked bool, chunk, chunks int) string {
	var b strings.Builder

	if settings.Mode == config.ModeComplete {
		completePreamble(&b, cat, chunked, chunk, chunks)
	} else {
		briefPreamble(&b, cat)
	}

	b.WriteString(`
- headline: at most 80 characters, plain and factual. No clickbait, no "here's why", no questions.
- summary: 1-3 sentences, 45 words maximum. What happened, and why it matters. Assume the reader has not followed the story, but do not pad with background.
- Never invent facts, numbers, or quotes. Many items give you only a headline; when that is all you have, keep the summary to what it supports.
- tag: one lower-case word or two, e.g. "world", "tech", "germany", "business", "science", "climate".
- importance: "high" for the handful of topics worth reading even on a busy morning, "normal" for the rest.
- source_indexes: the index of every item you used for that topic, from the numbered list.
`)

	if settings.Mode == config.ModeBrief {
		fmt.Fprintf(&b, "- Produce at most %d topics. If there is more news than that, drop the least important rather than shortening everything.\n", settings.MaxTopics)
	}
	fmt.Fprintf(&b, "- Write the brief in %s, regardless of the language of the source items.\n", cfg.Language)

	if len(cfg.Exclude.Topics) > 0 {
		b.WriteString("\nDrop entirely - do not summarise, do not mention, do not fold into another topic:\n")
		for _, t := range cfg.Exclude.Topics {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(t))
		}
		if settings.Mode == config.ModeComplete {
			b.WriteString("\nList the index of every item you drop for one of those reasons in excluded_indexes. That field is the only way to leave an item out; it exists so a deliberate drop can be told apart from a mistake.\n")
		}
	}
	if len(cfg.Exclude.Topics) == 0 && settings.Mode == config.ModeComplete {
		b.WriteString("\nLeave excluded_indexes empty. There is nothing you are allowed to drop.\n")
	}
	if settings.Mode == config.ModeBrief {
		b.WriteString("\nLeave excluded_indexes empty; in this brief, unused items are simply the ones that did not make the cut.\n")
	}
	return b.String()
}

// briefPreamble is the editor's brief: select, rank, and be willing to leave
// things out.
func briefPreamble(b *strings.Builder, cat string) {
	fmt.Fprintf(b, `You are the editor of one person's daily briefing on a single subject: %s. You receive raw items pulled from the feeds they follow on that subject over the last day, and turn them into a short brief they can skim over coffee.

Every item you are given belongs to this subject already, so do not set anything aside for being off-topic, niche, or too specialist. Judge importance within %s, not against the news of the world: for a %s brief, the biggest %s story of the day leads, even on a day when it would never make a general front page.

Rules:
- Group items covering the same story into ONE topic, even across different outlets. Merge aggressively.
- Order topics by how much they matter to someone who follows this subject closely. Most important first.`, cat, cat, cat, cat)
}

// completePreamble is the opposite job: the reader has said they would rather
// not have this subject edited for them, so the model is compiling a list, not
// choosing what is worth their time.
func completePreamble(b *strings.Builder, cat string, chunked bool, chunk, chunks int) {
	fmt.Fprintf(b, `You are compiling one person's complete reading list for a single subject: %s. They follow this subject closely and have asked not to miss anything, so this is not a selection - it is every item, written up.

Every item you are given belongs to this subject already. Nothing may be left out for being small, routine, repetitive, promotional, or not worth a general reader's time. That judgement is not yours to make here.

Rules:
- Every numbered item must appear in the source_indexes of exactly one topic. Account for all of them.
- Merge two items into ONE topic only when they are plainly the same story - the same announcement, race, incident or product. Otherwise give each item its own topic. When in doubt, keep them apart: two topics that turn out to be related is a small annoyance, a lost item is the failure this brief exists to prevent.
- Keep the topics in the order the items are listed, which is newest first.`, cat)

	if chunked {
		fmt.Fprintf(b, `
- This is part %d of %d for today, split only because the day was long. Cover the items in front of you and do not refer to the other parts.`, chunk+1, chunks)
	}
}

func userPrompt(items []feeds.Item, loc *time.Location, mode string) string {
	var b strings.Builder
	if mode == config.ModeComplete {
		fmt.Fprintf(&b, "Here are %d items from my feeds, indexed 0 to %d. Write up every one of them.\n\n", len(items), len(items)-1)
	} else {
		fmt.Fprintf(&b, "Here are %d items from my feeds. Build today's brief.\n\n", len(items))
	}

	for i, it := range items {
		fmt.Fprintf(&b, "[%d] %s (%s, %s)\n", i, it.Title, it.Feed, it.Published.In(loc).Format("Mon 15:04"))
		if it.Summary != "" {
			fmt.Fprintf(&b, "    %s\n", it.Summary)
		}
	}
	return b.String()
}

// --- the front page ---

// BriefErrPrefix marks a digest error as being about the front page rather
// than a feed or a category. Startup reads it to tell a day that never had a
// front page written from one whose front page was attempted and did not work,
// which are worth treating differently.
const BriefErrPrefix = "front page: "

// BackfillBrief writes the front page for a day that already has topics but no
// front page — one briefed before the front page existed, or one whose front
// page call failed.
//
// It is deliberately not a re-run: the front page is written from stored
// topics, so nothing has to be fetched and no category has to be briefed
// again. One Claude call instead of one per category, and because no topic is
// rebuilt, no topic ID changes and no read mark moves.
func (g *Generator) BackfillBrief(ctx context.Context, date string) (*store.Digest, error) {
	d, err := g.Store.LoadDigest(date)
	if err != nil {
		return nil, err
	}
	if d == nil || len(d.Topics) == 0 {
		return d, nil // nothing to write a front page from
	}

	brief, err := g.frontBrief(ctx, d.Topics)
	if err != nil {
		g.Log.Error("front page backfill failed", "date", date, "err", err)
	} else {
		g.Log.Info("front page backfilled", "date", date, "stories", len(brief))
	}

	// Saved either way: on failure the note is the point, since it is what
	// stops the next restart from paying for the same call again.
	noteBrief(d, brief, err)
	if saveErr := g.Store.SaveDigest(d); saveErr != nil {
		return nil, saveErr
	}
	return d, err
}

// noteBrief folds one front page attempt into the digest. An attempt that
// produced no stories is recorded like a failure rather than left silent: it
// is a completed call, and a day that looks like it was never tried would be
// tried again on every restart.
func noteBrief(d *store.Digest, brief []store.BriefStory, err error) {
	// The previous attempt's note goes either way — on success it is no longer
	// true, and on failure it is replaced by this attempt's.
	d.Errors = withoutBriefError(d.Errors)

	switch {
	case err != nil:
		d.Errors = append(d.Errors, BriefErrPrefix+err.Error())
	case len(brief) == 0:
		d.Errors = append(d.Errors, BriefErrPrefix+"produced no stories")
	default:
		d.Brief = brief
	}
}

func withoutBriefError(errs []string) []string {
	var out []string
	for _, e := range errs {
		if !strings.HasPrefix(e, BriefErrPrefix) {
			out = append(out, e)
		}
	}
	return out
}

// BriefAttempted reports whether a front page call has already been made for
// this day and did not produce one. A day with neither a front page nor such a
// note has simply never been asked.
func BriefAttempted(d *store.Digest) bool {
	for _, e := range d.Errors {
		if strings.HasPrefix(e, BriefErrPrefix) {
			return true
		}
	}
	return false
}

type modelBrief struct {
	Stories []struct {
		Lead         string `json:"lead"`
		Text         string `json:"text"`
		TopicIndexes []int  `json:"topic_indexes"`
	} `json:"stories"`
}

func briefSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"stories": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"lead": map[string]any{"type": "string"},
						"text": map[string]any{"type": "string"},
						"topic_indexes": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "integer"},
						},
					},
					"required":             []string{"lead", "text", "topic_indexes"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"stories"},
		"additionalProperties": false,
	}
}

// frontBrief writes the cross-category front page from topics that have already
// been briefed. It reads no feed items of its own, so it cannot introduce a
// fact none of the categories reported.
func (g *Generator) frontBrief(ctx context.Context, topics []store.Topic) ([]store.BriefStory, error) {
	raw, err := g.Backend.Complete(ctx, claude.Request{
		System: frontSystemPrompt(g.Cfg),
		User:   frontUserPrompt(topics),
		Model:  g.Cfg.Model,
		Effort: g.Cfg.Effort,
		Schema: briefSchema(),
	})
	if err != nil {
		return nil, err
	}

	jsonText, err := claude.ExtractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w (got %.200q)", err, raw)
	}
	var parsed modelBrief
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w (got %.200q)", err, jsonText)
	}

	out := make([]store.BriefStory, 0, len(parsed.Stories))
	for _, s := range parsed.Stories {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		var ids []string
		seen := map[string]bool{}
		for _, idx := range s.TopicIndexes {
			if idx < 0 || idx >= len(topics) || seen[topics[idx].ID] {
				continue
			}
			seen[topics[idx].ID] = true
			ids = append(ids, topics[idx].ID)
		}
		out = append(out, store.BriefStory{
			Lead:     strings.TrimSpace(s.Lead),
			Text:     text,
			TopicIDs: ids,
		})
	}
	return out, nil
}

func frontSystemPrompt(cfg *config.Config) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are writing the front page of one person's morning briefing: the handful of things they should know before they read anything else. The form is the one The Economist uses in "The World in Brief" - a short run of dense paragraphs, each opening on the country, company, person or institution the sentence is about.

What you are given is not a newswire. It is the finished brief from every subject this person follows: %s. The front page is theirs, not a general newspaper's, and that is the whole point of it - a serious story in one of their own subjects belongs here on the day it breaks, ahead of a routine story from the news. Do not fill the page with world news out of habit, and do not pad it with a specialist story on a day when nothing much happened there.

Rules:
- At most %d paragraphs, in descending order of how much they matter to this reader.
- lead: the subject the paragraph opens on, 1-3 words. It is the grammatical subject of the first sentence, not a label or a category name.
- text: the rest of that first sentence, then one to three more. 70 words maximum. Do not repeat the lead - the page prints the lead and your text as one continuous sentence, so "struck two rocket launchers near the strait" follows a lead of "America".
- Write only from the topics given. Every fact must already be in one of them. Do not add background, context or numbers of your own.
- Where several topics are the same story, write one paragraph and cite them all.
- topic_indexes: the index of every topic the paragraph draws on.
`, strings.Join(cfg.CategoryNames(), ", "), cfg.Brief.MaxItems)

	fmt.Fprintf(&b, "- Write in %s.\n", cfg.Language)
	return b.String()
}

func frontUserPrompt(topics []store.Topic) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here is everything briefed this morning, across %d topics. Write the front page.\n\n", len(topics))

	for i, t := range topics {
		mark := ""
		if t.Important {
			mark = ", flagged important by its own brief"
		}
		fmt.Fprintf(&b, "[%d] (%s%s) %s\n", i, t.Category, mark, t.Headline)
		if t.Summary != "" {
			fmt.Fprintf(&b, "    %s\n", t.Summary)
		}
	}
	return b.String()
}
