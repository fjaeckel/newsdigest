// Package store persists digests and read state as plain JSON files on disk.
// No database: one file per day plus a single read-state file.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Source is one article backing a topic.
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Feed  string `json:"feed"`
}

// Topic is one merged story in the brief.
type Topic struct {
	ID string `json:"id"`
	// Category is the feed category this topic was briefed from, and decides
	// which standing feed it belongs to. Empty on digests written before
	// categories existed.
	Category  string   `json:"category,omitempty"`
	Headline  string   `json:"headline"`
	Summary   string   `json:"summary"`
	Tag       string   `json:"tag"`
	Important bool     `json:"important"`
	Sources   []Source `json:"sources"`
}

// Stats is the small "how did this get made" footer.
type Stats struct {
	Feeds        int `json:"feeds"`
	FeedsFailed  int `json:"feeds_failed"`
	Items        int `json:"items"`
	FilteredOut  int `json:"filtered_out"`
	Topics       int `json:"topics"`
	DurationSecs int `json:"duration_secs"`
}

// CategoryStat is the per-category audit trail. Covered against Items is the
// number that matters for a complete category: it is the claim that nothing
// was missed, stated rather than assumed.
type CategoryStat struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Items    int    `json:"items"`
	Topics   int    `json:"topics"`
	Covered  int    `json:"covered"`  // items referenced by at least one topic
	Excluded int    `json:"excluded"` // items the model dropped under an exclude rule
	Rescued  int    `json:"rescued"`  // items it silently lost, added back as their own topics
}

// BriefStory is one paragraph of the cross-category front page, in the shape
// The Economist's World in Brief uses: a bolded subject, then the sentence that
// subject starts. TopicIDs are the topics it was written from, which is what
// lets the page show real sources under a paragraph the model wrote.
type BriefStory struct {
	Lead     string   `json:"lead"`
	Text     string   `json:"text"`
	TopicIDs []string `json:"topic_ids"`
}

// Digest is one morning's brief.
type Digest struct {
	Date        string         `json:"date"` // YYYY-MM-DD in the configured timezone
	GeneratedAt time.Time      `json:"generated_at"`
	Model       string         `json:"model"`
	Backend     string         `json:"backend"`
	Stats       Stats          `json:"stats"`
	Categories  []CategoryStat `json:"categories,omitempty"`
	Brief       []BriefStory   `json:"brief,omitempty"`
	Topics      []Topic        `json:"topics"`
	Errors      []string       `json:"errors,omitempty"`
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Store owns a data directory. All methods are safe for concurrent use.
type Store struct {
	dir string

	mu   sync.RWMutex
	read map[string][]string // date -> read topic IDs
}

// New opens (and creates if needed) a store rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "digests"), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{dir: dir, read: map[string][]string{}}
	if err := s.loadRead(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) digestPath(date string) string {
	return filepath.Join(s.dir, "digests", date+".json")
}

func (s *Store) readPath() string { return filepath.Join(s.dir, "read.json") }

// SaveDigest writes a digest, replacing any existing one for that date.
func (s *Store) SaveDigest(d *Digest) error {
	if !dateRE.MatchString(d.Date) {
		return fmt.Errorf("invalid digest date %q", d.Date)
	}
	return writeJSON(s.digestPath(d.Date), d)
}

// LoadDigest returns the digest for date, or nil if there isn't one.
func (s *Store) LoadDigest(date string) (*Digest, error) {
	if !dateRE.MatchString(date) {
		return nil, nil
	}
	raw, err := os.ReadFile(s.digestPath(date))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d Digest
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("corrupt digest %s: %w", date, err)
	}
	return &d, nil
}

// Dates lists every stored digest date, newest first.
func (s *Store) Dates() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "digests"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		date := name[:len(name)-len(".json")]
		if dateRE.MatchString(date) {
			out = append(out, date)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// Latest returns the most recent digest, or nil if none exist yet.
func (s *Store) Latest() (*Digest, error) {
	dates, err := s.Dates()
	if err != nil || len(dates) == 0 {
		return nil, err
	}
	return s.LoadDigest(dates[0])
}

// --- read state ---

func (s *Store) loadRead() error {
	raw, err := os.ReadFile(s.readPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// A corrupt read file must not take the app down; losing read marks is
	// annoying, not fatal.
	var state map[string][]string
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil
	}
	s.read = state
	return nil
}

// ReadSet returns the set of read topic IDs for a date.
func (s *Store) ReadSet(date string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := make(map[string]bool, len(s.read[date]))
	for _, id := range s.read[date] {
		set[id] = true
	}
	return set
}

// SetRead marks a single topic read or unread.
func (s *Store) SetRead(date, topicID string, read bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := s.read[date]
	idx := -1
	for i, id := range ids {
		if id == topicID {
			idx = i
			break
		}
	}
	switch {
	case read && idx == -1:
		s.read[date] = append(ids, topicID)
	case !read && idx >= 0:
		s.read[date] = append(ids[:idx], ids[idx+1:]...)
	default:
		return nil // already in the requested state
	}
	return s.persistReadLocked()
}

// SetManyRead marks a subset of one day's topics read or unread, leaving every
// other topic on that day alone. SetAllRead replaces the day wholesale, which
// is right when the caller owns the whole day but destructive for a subset:
// clearing one category would drop the read marks of every other category
// briefed on the same date.
func (s *Store) SetManyRead(date string, topicIDs []string, read bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(topicIDs) == 0 {
		return nil
	}

	subset := make(map[string]bool, len(topicIDs))
	for _, id := range topicIDs {
		subset[id] = true
	}

	var next []string
	if read {
		seen := make(map[string]bool, len(s.read[date]))
		for _, id := range s.read[date] {
			seen[id] = true
			next = append(next, id)
		}
		for _, id := range topicIDs {
			if !seen[id] {
				seen[id] = true
				next = append(next, id)
			}
		}
	} else {
		for _, id := range s.read[date] {
			if !subset[id] {
				next = append(next, id)
			}
		}
	}

	if len(next) == 0 {
		delete(s.read, date)
	} else {
		s.read[date] = next
	}
	return s.persistReadLocked()
}

// SetAllRead marks every given topic ID read (or clears the date entirely).
// The caller must own the whole day; use SetManyRead for a subset.
func (s *Store) SetAllRead(date string, topicIDs []string, read bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !read {
		delete(s.read, date)
	} else {
		s.read[date] = append([]string(nil), topicIDs...)
	}
	return s.persistReadLocked()
}

func (s *Store) persistReadLocked() error {
	return writeJSON(s.readPath(), s.read)
}

// Prune deletes digests older than keepDays and their read state. keepDays <= 0
// disables pruning.
func (s *Store) Prune(keepDays int, now time.Time) error {
	if keepDays <= 0 {
		return nil
	}
	cutoff := now.AddDate(0, 0, -keepDays).Format("2006-01-02")

	dates, err := s.Dates()
	if err != nil {
		return err
	}
	for _, date := range dates {
		if date >= cutoff {
			continue
		}
		if err := os.Remove(s.digestPath(date)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		s.mu.Lock()
		delete(s.read, date)
		err = s.persistReadLocked()
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeJSON writes v atomically: temp file in the same directory, then rename,
// so a crash mid-write can never leave a half-written digest behind.
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
