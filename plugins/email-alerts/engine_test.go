package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

func TestLastBoundaryDaily(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 25, 17, 30, 0, 0, loc)
	got := lastBoundary(now, loc, -1, 18)
	want := time.Date(2026, 10, 24, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("before the hour: got %v want %v", got, want)
	}
	now = time.Date(2026, 10, 25, 18, 0, 0, 0, loc)
	if got := lastBoundary(now, loc, -1, 18); !got.Equal(now) {
		t.Fatalf("on the hour: got %v want %v", got, now)
	}
}

func TestLastBoundaryWeekly(t *testing.T) {
	loc := time.UTC
	// 2026-10-25 is a Sunday; Friday 18:00 before it is 2026-10-23.
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, loc)
	got := lastBoundary(now, loc, 5, 18)
	want := time.Date(2026, 10, 23, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	next := nextBoundary(now, loc, 5, 18)
	if want := time.Date(2026, 10, 30, 18, 0, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("next: got %v want %v", next, want)
	}
}

func TestIsDue(t *testing.T) {
	s := defaultSettings()
	s.Schedule.Timezone = "UTC"
	now := time.Date(2026, 10, 25, 19, 0, 0, 0, time.UTC)
	if !isDue("daily", now.Add(-26*time.Hour), now, s) {
		t.Fatal("daily digest last run yesterday morning should be due after 18:00")
	}
	if isDue("daily", now.Add(-30*time.Minute), now, s) {
		t.Fatal("daily digest that ran at 18:30 should not be due again")
	}
	if !isDue("instant", now, now, s) {
		t.Fatal("instant is always due")
	}
}

func card(id, typ string, added time.Time) Card {
	return Card{ContentID: id, Type: typ, AddedAt: added.Format(time.RFC3339Nano)}
}

func TestPickForFiltersAndDedupes(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)
	entries := []Entry{
		{Card: card("a", "episode", now.Add(-10*time.Minute)), Library: LibraryRef{ID: "tv"}},
		{Card: card("b", "movie", now.Add(-10*time.Minute)), Library: LibraryRef{ID: "films"}},
		{Card: card("c", "episode", now.Add(-1*time.Minute)), Library: LibraryRef{ID: "tv"}},  // inside settle window
		{Card: card("d", "episode", now.Add(-48*time.Hour)), Library: LibraryRef{ID: "tv"}},   // before since
		{Card: card("e", "episode", now.Add(-20*time.Minute)), Library: LibraryRef{ID: "tv"}}, // already sent
	}
	j := job{
		r:     Recipient{Types: []string{"episode"}},
		st:    RecipientState{Recent: []string{"e"}},
		since: now.Add(-3 * time.Hour),
	}
	got := pickFor(j, entries, cutoff)
	if len(got) != 1 || got[0].Card.ContentID != "a" {
		ids := []string{}
		for _, e := range got {
			ids = append(ids, e.Card.ContentID)
		}
		t.Fatalf("picked %v, want [a]", ids)
	}
	j.r = Recipient{Libraries: []string{"films"}}
	got = pickFor(j, entries, cutoff)
	if len(got) != 1 || got[0].Card.ContentID != "b" {
		t.Fatalf("library filter picked %d items", len(got))
	}
}

func TestDefaultTemplatesRenderSample(t *testing.T) {
	s := defaultSettings()
	env := RenderEnv{Settings: s, ServerName: "Silo", PublicURL: "https://silo.example.com", Mode: "instant", Now: time.Now(),
		Recipient: Recipient{Name: "Pete Smith", Email: "p@example.com"}, UnsubscribeURL: "https://silo.example.com/u?t=1"}
	r, err := Render(context.Background(), env, sampleEntries(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Harbour Lights", "S02E03", "The Lantern Room", "fog-bound fishing town", "The Long Way North", "Hi Pete", "https://silo.example.com/item/movie:sample-3", "Unsubscribe"} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("HTML missing %q", want)
		}
		if !strings.Contains(r.Text, want) && want != "Hi Pete" && want != "Unsubscribe" {
			t.Errorf("text missing %q", want)
		}
	}
	if !strings.Contains(r.Subject, "2 new episodes") || !strings.Contains(r.Subject, "1 new movie") {
		t.Errorf("subject = %q", r.Subject)
	}
	if strings.Contains(r.HTML, "{{") {
		t.Error("HTML contains unrendered tags")
	}
}

func TestSingleItemSubject(t *testing.T) {
	env := RenderEnv{Settings: defaultSettings(), ServerName: "Silo", Mode: "instant", Now: time.Now()}
	r, err := Render(context.Background(), env, sampleEntries()[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Subject != "New on Silo: Harbour Lights · S02E03 · The Lantern Room" {
		t.Fatalf("subject = %q", r.Subject)
	}
}

func TestEmbedsOnlyUsedImagesAndDropsFailures(t *testing.T) {
	s := defaultSettings()
	entries := sampleEntries()
	entries[2].Card.PosterURL = "/img/good.jpg"
	entries[2].Card.BackdropURL = "/img/unused-in-template.jpg"
	entries[0].Series.PosterURL = "/img/broken.jpg"
	entries[1].Series = entries[0].Series
	fetch := func(_ context.Context, raw string) ([]byte, string, error) {
		if strings.Contains(raw, "broken") {
			return nil, "", errors.New("404")
		}
		return []byte{0xff, 0xd8, 0xff}, "image/jpeg", nil
	}
	env := RenderEnv{Settings: s, ServerName: "Silo", Mode: "instant", Now: time.Now()}
	r, err := Render(context.Background(), env, entries, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Images) != 1 {
		t.Fatalf("embedded %d images, want 1", len(r.Images))
	}
	if !strings.Contains(r.HTML, "cid:"+r.Images[0].CID) {
		t.Fatal("embedded image not referenced")
	}
	if n := len(cidPattern.FindAllString(r.HTML, -1)); n != 1 {
		t.Fatalf("HTML references %d cids, want 1 (broken poster should be dropped)", n)
	}
}

func TestCustomVarsAndThemeVariables(t *testing.T) {
	s := defaultSettings()
	s.CustomVars = []CustomVar{{Key: "signoff", Value: "Happy watching, Pete"}}
	s.Templates.Text = "{{header_title}} / {{signoff}} / {{custom.signoff}}"
	env := RenderEnv{Settings: s, ServerName: "Locky Media", Mode: "daily", Now: time.Now()}
	r, err := Render(context.Background(), env, sampleEntries(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(r.Text); got != "New on Locky Media / Happy watching, Pete / Happy watching, Pete" {
		t.Fatalf("text = %q", got)
	}
}

func TestTemplateErrorIsReported(t *testing.T) {
	s := defaultSettings()
	s.Templates.HTML = "{{#shows}}unclosed"
	if err := validateTemplates(s); err == nil || !strings.Contains(err.Error(), "HTML template") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopbackURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://:8080":         "http://127.0.0.1:8080",
		"http://0.0.0.0:8080":  "http://127.0.0.1:8080",
		"http://10.0.0.5:8080": "http://10.0.0.5:8080",
	} {
		if got := loopbackURL(in); got != want {
			t.Errorf("loopbackURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitlesAreLinksWithoutButtons(t *testing.T) {
	env := RenderEnv{Settings: defaultSettings(), ServerName: "Silo", PublicURL: "https://silo.example.com", Mode: "instant", Now: time.Now()}
	r, err := Render(context.Background(), env, sampleEntries(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "Watch now") || strings.Contains(r.Text, "Watch now") {
		t.Fatal("email still has a Watch now button")
	}
	for _, link := range []string{
		`<a href="https://silo.example.com/item/series:sample-harbour-lights"`,
		`<a href="https://silo.example.com/item/episode:sample-1"`,
		`<a href="https://silo.example.com/item/movie:sample-3"`,
	} {
		if !strings.Contains(r.HTML, link) {
			t.Errorf("HTML missing title link %s", link)
		}
	}
}

func TestTriggerCoalescesEvents(t *testing.T) {
	a := newApp(hclog.NewNullLogger())
	timer := func() *time.Timer {
		a.triggerMu.Lock()
		defer a.triggerMu.Unlock()
		return a.eventTimer
	}
	a.trigger(0)
	first := timer()
	a.trigger(0)
	a.trigger(0)
	if timer() != first || first == nil {
		t.Fatal("repeated events should share one scheduled run")
	}
	time.Sleep(eventBatch + time.Second)
	if timer() != nil {
		t.Fatal("timer should clear after the run so the next event schedules again")
	}
	if r := a.report(); r == nil || r.Reason != "new media event" {
		t.Fatalf("run report = %+v, want a new media event run", r)
	}
}

func TestInstantRateLimit(t *testing.T) {
	s := defaultSettings()
	s.Schedule.Timezone = "UTC"
	s.Delivery.InstantGapMinutes = 10
	now := time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)

	if !instantHeldUntil(RecipientState{}, s, now).IsZero() {
		t.Fatal("first email should go straight out")
	}
	st := RecipientState{LastInstant: now.Add(-4 * time.Minute)}
	if got, want := instantHeldUntil(st, s, now), now.Add(6*time.Minute); !got.Equal(want) {
		t.Fatalf("inside the gap: held until %v, want %v", got, want)
	}
	st.LastInstant = now.Add(-11 * time.Minute)
	if !instantHeldUntil(st, s, now).IsZero() {
		t.Fatal("after the gap the next email should go")
	}

	s.Delivery.InstantDailyCap = 3
	st = RecipientState{LastInstant: now.Add(-time.Hour), Day: "2026-10-25", DayCount: 3}
	if got, want := instantHeldUntil(st, s, now), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("daily cap: held until %v, want %v", got, want)
	}
	st.Day = "2026-10-24"
	if !instantHeldUntil(st, s, now).IsZero() {
		t.Fatal("yesterday's count shouldn't hold today's email")
	}

	s.Delivery.InstantGapMinutes, s.Delivery.InstantDailyCap = 0, 0
	st = RecipientState{LastInstant: now, Day: "2026-10-25", DayCount: 99}
	if !instantHeldUntil(st, s, now).IsZero() {
		t.Fatal("with both limits off nothing is held")
	}
}
