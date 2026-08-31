package main

import (
	"testing"
	"time"

	"github.com/fjaeckel/newsdigest/internal/digest"
	"github.com/fjaeckel/newsdigest/internal/store"
)

func TestShouldBriefOnStartup(t *testing.T) {
	runAt := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	before := runAt.Add(-2 * time.Hour)
	after := runAt.Add(2 * time.Hour)

	empty := &store.Digest{Date: "2026-08-27"}
	briefed := &store.Digest{Date: "2026-08-27", Topics: []store.Topic{
		{ID: "a", Category: "news", Headline: "Something happened"},
	}}
	// A digest written before feeds carried categories: topics, but no
	// category on any of them.
	legacy := &store.Digest{Date: "2026-08-27", Topics: []store.Topic{
		{ID: "a", Headline: "Something happened"},
		{ID: "b", Headline: "Something else happened"},
	}}

	cases := []struct {
		name     string
		existing *store.Digest
		now      time.Time
		want     bool
	}{
		// Nothing has run today: brief now rather than show an empty page for
		// hours. The clock does not get a say.
		{"never ran, before run_at", nil, before, true},
		{"never ran, after run_at", nil, after, true},

		// A completed run that found nothing is a real result, so the guard
		// applies to retrying it.
		{"ran empty, before run_at", empty, before, false},
		{"ran empty, after run_at", empty, after, true},

		// Anything already briefed is left alone, whatever the time.
		{"already briefed, before run_at", briefed, before, false},
		{"already briefed, after run_at", briefed, after, false},

		// Written before feeds had categories: every topic would fall back to
		// "general" and every standing feed would read empty, so re-brief it
		// once regardless of the clock.
		{"predates categories, before run_at", legacy, before, true},
		{"predates categories, after run_at", legacy, after, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := shouldBriefOnStartup(c.existing, c.now, runAt)
			if got != c.want {
				t.Errorf("shouldBriefOnStartup = %v (%s), want %v", got, reason, c.want)
			}
			if reason == "" {
				t.Error("decision came back without a reason to log")
			}
		})
	}
}

func TestShouldBackfillBrief(t *testing.T) {
	runAt := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	before := runAt.Add(-2 * time.Hour)
	after := runAt.Add(2 * time.Hour)

	topics := []store.Topic{{ID: "a", Category: "news", Headline: "Something happened"}}

	// Briefed before the front page existed: topics, no front page, and no note
	// saying one was ever attempted.
	noFront := &store.Digest{Date: "2026-08-27", Topics: topics}
	// The call ran this morning and came back broken, or empty.
	failed := &store.Digest{Date: "2026-08-27", Topics: topics,
		Errors: []string{"road.cc: http 503", digest.BriefErrPrefix + "produced no stories"}}
	done := &store.Digest{Date: "2026-08-27", Topics: topics,
		Brief: []store.BriefStory{{Lead: "America", Text: "did something."}}}
	// A morning that found nothing at all: the full-run path owns this case.
	empty := &store.Digest{Date: "2026-08-27"}

	cases := []struct {
		name     string
		existing *store.Digest
		briefOn  bool
		now      time.Time
		want     bool
	}{
		// Nothing to retry, so the clock does not get a say: one call turns a
		// blank front page into a real one.
		{"no front page yet, before run_at", noFront, true, before, true},
		{"no front page yet, after run_at", noFront, true, after, true},

		// A completed attempt is a real result, so retrying it is guarded the
		// way an empty morning is - otherwise every restart spends a call.
		{"attempt failed, before run_at", failed, true, before, false},
		{"attempt failed, after run_at", failed, true, after, true},

		{"already has one", done, true, after, false},
		{"switched off in config", noFront, false, after, false},

		// Never the thing that briefs a morning: with no topics there is
		// nothing to write a front page from.
		{"no topics at all", empty, true, after, false},
		{"nothing synced today", nil, true, after, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := shouldBackfillBrief(c.existing, c.briefOn, c.now, runAt)
			if got != c.want {
				t.Errorf("shouldBackfillBrief = %v (%s), want %v", got, reason, c.want)
			}
			if reason == "" {
				t.Error("decision came back without a reason to log")
			}
		})
	}
}

// The two startup decisions must not both fire: a day the full run is about to
// rebuild has no business being backfilled first.
func TestBackfillNeverOverlapsAFullRun(t *testing.T) {
	runAt := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)

	for _, now := range []time.Time{runAt.Add(-2 * time.Hour), runAt.Add(2 * time.Hour)} {
		for name, d := range map[string]*store.Digest{
			"nothing synced": nil,
			"ran empty":      {Date: "2026-08-27"},
			"predates categories": {Date: "2026-08-27", Topics: []store.Topic{
				{ID: "a", Headline: "Uncategorised"},
			}},
		} {
			full, _ := shouldBriefOnStartup(d, now, runAt)
			back, reason := shouldBackfillBrief(d, true, now, runAt)
			if full && back {
				t.Errorf("%s at %s: both a full run and a backfill fired (%s)",
					name, now.Format("15:04"), reason)
			}
		}
	}
}

// A day with topics is never regenerated on startup, even when some categories
// are empty: a quiet category is normal, and treating it as a gap would re-run
// the model on every restart.
func TestUnevenCategoriesDoNotTriggerAStartupRun(t *testing.T) {
	runAt := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	lopsided := &store.Digest{Date: "2026-08-27", Topics: []store.Topic{
		{ID: "a", Category: "news", Headline: "Only news ran today"},
	}}

	if got, reason := shouldBriefOnStartup(lopsided, runAt.Add(3*time.Hour), runAt); got {
		t.Errorf("a lopsided day triggered a re-run (%s)", reason)
	}
}
