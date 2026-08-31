package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feeds.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const feeds = `
feeds:
  - name: Tagesschau
    category: news
    url: https://example.com/news
  - name: Air Facts
    category: aviation
    url: https://example.com/av
`

// The `categories:` block is optional, and leaving it out has to keep the old
// behaviour exactly.
func TestCategoriesDefaultToBrief(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\nmax_topics: 9\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"news", "aviation"} {
		got := cfg.CategoryOf(name)
		if got.Mode != ModeBrief {
			t.Errorf("%s mode = %q, want %q", name, got.Mode, ModeBrief)
		}
		if got.GroupBy != GroupByImportance {
			t.Errorf("%s group_by = %q, want %q", name, got.GroupBy, GroupByImportance)
		}
		if got.MaxTopics != 9 {
			t.Errorf("%s max_topics = %d, want the top-level budget", name, got.MaxTopics)
		}
	}
}

// A complete category is a reading list, and reading lists are read outlet by
// outlet — so group_by follows from mode without having to be written down.
func TestCompleteImpliesGroupByFeed(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\nmax_topics: 9\ncategories:\n  aviation:\n    mode: complete\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CategoryOf("aviation"); got.GroupBy != GroupByFeed {
		t.Errorf("group_by = %q, want %q", got.GroupBy, GroupByFeed)
	}
	// Its neighbour is unaffected.
	if got := cfg.CategoryOf("news"); got.Mode != ModeBrief || got.GroupBy != GroupByImportance {
		t.Errorf("news picked up aviation's settings: %+v", got)
	}
	// And it can still be overridden the other way.
	cfg, err = load(t, "timezone: UTC\ncategories:\n  aviation:\n    mode: complete\n    group_by: importance\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CategoryOf("aviation"); got.GroupBy != GroupByImportance {
		t.Errorf("explicit group_by was overwritten: %q", got.GroupBy)
	}
}

func TestPerCategoryMaxTopicsOverridesTheDefault(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\nmax_topics: 9\ncategories:\n  news:\n    max_topics: 20\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CategoryOf("news").MaxTopics; got != 20 {
		t.Errorf("news max_topics = %d, want 20", got)
	}
	if got := cfg.CategoryOf("aviation").MaxTopics; got != 9 {
		t.Errorf("aviation max_topics = %d, want the inherited 9", got)
	}
}

// A typo in a mode is a config mistake worth failing on at startup, not
// something to discover from a strangely short brief a week later.
func TestBadModeAndGroupByAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"mode":     "timezone: UTC\ncategories:\n  news:\n    mode: everything\n" + feeds,
		"group_by": "timezone: UTC\ncategories:\n  news:\n    group_by: colour\n" + feeds,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, body)
			if err == nil {
				t.Fatalf("bad %s was accepted", name)
			}
			if !strings.Contains(err.Error(), "news") {
				t.Errorf("error does not name the offending category: %v", err)
			}
		})
	}
}

// The names are matched case-insensitively, like feed categories are.
func TestCategoryNamesAreNormalised(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\ncategories:\n  Aviation:\n    mode: complete\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CategoryOf("aviation").Mode; got != ModeComplete {
		t.Errorf("mode = %q; a capitalised key did not match its feeds", got)
	}
}

// A category that only exists in old stored digests still resolves, so its
// pages render instead of blowing up.
func TestUnknownCategoryGetsDefaults(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\nmax_topics: 9\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CategoryOf("cycling"); got.Mode != ModeBrief || got.MaxTopics != 9 {
		t.Errorf("unknown category = %+v, want the defaults", got)
	}
}

func TestFeedOrderFollowsTheConfig(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\n"+feeds+
		"  - name: ForeFlight\n    category: aviation\n    url: https://example.com/ff\n")
	if err != nil {
		t.Fatal(err)
	}
	order := cfg.FeedOrder("aviation")
	if order["Air Facts"] != 0 || order["ForeFlight"] != 1 {
		t.Errorf("feed order = %v", order)
	}
	if _, ok := order["Tagesschau"]; ok {
		t.Error("another category's feed leaked into the order")
	}
}

func TestBriefDefaultsOnWithEightItems(t *testing.T) {
	cfg, err := load(t, "timezone: UTC\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Brief.On() || cfg.Brief.MaxItems != 8 {
		t.Errorf("brief defaults = %+v", cfg.Brief)
	}

	cfg, err = load(t, "timezone: UTC\nbrief:\n  enabled: false\n"+feeds)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Brief.On() {
		t.Error("brief: enabled: false was ignored")
	}
}
