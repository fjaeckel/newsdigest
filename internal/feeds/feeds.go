// Package feeds fetches and normalises RSS/Atom items.
package feeds

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/fjaeckel/newsdigest/internal/config"
)

// Item is one article, flattened to what the summariser actually needs.
type Item struct {
	Title     string
	Link      string
	Summary   string
	Feed      string
	Published time.Time
}

// Result carries the items plus any per-feed failures, so a dead feed shows up
// in the digest instead of silently shrinking it.
type Result struct {
	Items    []Item
	Errors   []string
	FeedsOK  int
	Excluded int
}

const (
	fetchTimeout    = 30 * time.Second
	maxSummaryRunes = 600
	concurrency     = 8
)

var (
	tagRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE = regexp.MustCompile(`\s+`)
)

// Fetch pulls every configured feed concurrently and returns the items
// published within the lookback window, newest first.
func Fetch(ctx context.Context, cfg *config.Config) Result {
	cutoff := time.Now().Add(-time.Duration(cfg.LookbackHours) * time.Hour)

	var (
		mu   sync.Mutex
		res  Result
		wg   sync.WaitGroup
		gate = make(chan struct{}, concurrency)
	)

	client := &http.Client{Timeout: fetchTimeout}

	for _, f := range cfg.Feeds {
		wg.Add(1)
		go func(f config.Feed) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			items, err := fetchOne(ctx, client, f, cutoff, cfg.MaxItemsPerFeed)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", f.Name, err))
				return
			}
			res.FeedsOK++
			res.Items = append(res.Items, items...)
		}(f)
	}
	wg.Wait()

	sort.Slice(res.Items, func(i, j int) bool {
		return res.Items[i].Published.After(res.Items[j].Published)
	})
	sort.Strings(res.Errors)

	before := len(res.Items)
	res.Items = dropExcluded(res.Items, cfg.Exclude.Keywords)
	res.Excluded = before - len(res.Items)

	if cfg.MaxItemsTotal > 0 && len(res.Items) > cfg.MaxItemsTotal {
		res.Items = res.Items[:cfg.MaxItemsTotal]
	}
	return res
}

func fetchOne(ctx context.Context, client *http.Client, f config.Feed, cutoff time.Time, limit int) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	// Some publishers serve 403 to clients without a recognisable UA.
	req.Header.Set("User-Agent", "newsdigest/1.0 (+https://github.com/fjaeckel/newsdigest)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	parsed, err := gofeed.NewParser().Parse(resp.Body)
	if err != nil {
		return nil, err
	}

	var out []Item
	for _, e := range parsed.Items {
		if e == nil {
			continue
		}
		published := publishedAt(e)
		if published.Before(cutoff) {
			continue
		}
		title := clean(e.Title)
		if title == "" {
			continue
		}
		out = append(out, Item{
			Title:     title,
			Link:      strings.TrimSpace(e.Link),
			Summary:   truncate(clean(firstNonEmpty(e.Description, e.Content)), maxSummaryRunes),
			Feed:      f.Name,
			Published: published,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// publishedAt prefers the published date but falls back to updated, and finally
// to now so that feeds without timestamps still make it into today's digest.
func publishedAt(e *gofeed.Item) time.Time {
	if e.PublishedParsed != nil {
		return *e.PublishedParsed
	}
	if e.UpdatedParsed != nil {
		return *e.UpdatedParsed
	}
	return time.Now()
}

func dropExcluded(items []Item, keywords []string) []Item {
	if len(keywords) == 0 {
		return items
	}
	kept := items[:0]
	for _, it := range items {
		haystack := strings.ToLower(it.Title + " " + it.Summary)
		blocked := false
		for _, k := range keywords {
			if k != "" && strings.Contains(haystack, k) {
				blocked = true
				break
			}
		}
		if !blocked {
			kept = append(kept, it)
		}
	}
	return kept
}

func clean(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
