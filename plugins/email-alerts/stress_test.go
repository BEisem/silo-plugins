package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func hostileEntries() []Entry {
	ep := func(n int) *int { return &n }
	evil := `<script>alert(1)</script>"><img src=x onerror=alert(2)>{{#x}}{{{raw}}}{{> p}}` + "\r\nBcc: evil@example.com"
	huge := strings.Repeat("A very long synopsis. ", 20000)
	var out []Entry
	for i := 0; i < 5; i++ {
		out = append(out, Entry{Card: Card{ContentID: fmt.Sprintf("episode:%d", i), Type: "episode", Title: evil, Overview: huge,
			SeriesID: "series:x", SeriesTitle: evil, SeasonNumber: ep(1), EpisodeNumber: ep(i + 1),
			AddedAt: time.Now().Format(time.RFC3339Nano), PosterURL: "javascript:alert(1)", Genres: []string{evil}}})
	}
	out = append(out, Entry{Card: Card{ContentID: "movie:\"><b>", Type: "movie", Title: evil, Overview: evil, Year: -5,
		AddedAt: "not a date"}})
	out = append(out, Entry{Card: Card{}}) // an entirely empty card
	return out
}

func TestRenderHostileInput(t *testing.T) {
	s := defaultSettings()
	env := RenderEnv{Settings: s, ServerName: evilName(), PublicURL: "https://silo.example.com", Mode: "instant", Now: time.Now(),
		Recipient: Recipient{Name: evilName(), Email: "p@example.com"}, UnsubscribeURL: "https://silo.example.com/u?t=1",
		PluginBase: "https://silo.example.com/plugin"}
	for _, mode := range []string{"instant", "daily", "weekly", "test", "preview"} {
		env.Mode = mode
		r, err := Render(context.Background(), env, hostileEntries(), nil)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if strings.Contains(r.HTML, "<script>alert") || strings.Contains(r.HTML, "<img src=x") {
			t.Errorf("%s: unescaped HTML injected into the email", mode)
		}
		if strings.ContainsAny(r.Subject, "\r\n") {
			t.Errorf("%s: subject has a line break: %q", mode, r.Subject)
		}
		if strings.Contains(r.HTML, "javascript:alert") {
			t.Errorf("%s: javascript: URL reached the HTML", mode)
		}
	}
}

func evilName() string { return `<b>x</b>"` + "\r\nBcc: a@b.c" }

func TestBuildMsgHeaderInjection(t *testing.T) {
	cfg := SMTPConfig{Host: "h", FromAddress: "silo@example.com", FromName: evilName(), ReplyTo: "r@example.com"}
	for _, o := range []Outgoing{
		{To: "a@example.com", ToName: evilName(), Rendered: &Rendered{Subject: "Hi\r\nBcc: evil@example.com", HTML: "<p>x</p>", Text: "x"}},
		{To: "a@example.com", Rendered: &Rendered{Subject: "ok", HTML: "<p>x</p>", Text: "x"}, UnsubscribeURL: "https://x/u?t=1\r\nBcc: evil@example.com"},
	} {
		m, err := buildMsg(cfg, o)
		if err != nil {
			continue // refusing is fine
		}
		var buf bytes.Buffer
		if _, err := m.WriteTo(&buf); err != nil {
			continue
		}
		head, _, _ := strings.Cut(buf.String(), "\r\n\r\n")
		for _, line := range strings.Split(head, "\r\n") {
			if strings.HasPrefix(strings.ToLower(line), "bcc:") {
				t.Fatalf("header injection: %q", line)
			}
		}
	}
}

func TestSameHostIsExact(t *testing.T) {
	base := "https://silo.example.com"
	for raw, want := range map[string]bool{
		"/api/v2/artwork/x":                                    true,
		"https://silo.example.com/api/x":                       true,
		"https://silo.example.com":                             true,
		"https://silo.example.com.evil.test/x":                 false,
		"https://silo.example.com@evil.test/x":                 false,
		"https://silo.example.comevil.test/x":                  false,
		"http://silo.example.com/x":                            false,
		"https://cdn.example.net/x?u=https://silo.example.com": false,
	} {
		if got := sameHost(base, raw); got != want {
			t.Errorf("sameHost(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseConfigOddTypes(t *testing.T) {
	mk := func(k string, m map[string]any) *pluginv1.ConfigEntry {
		v, _ := structpb.NewStruct(m)
		return &pluginv1.ConfigEntry{Key: k, Value: v}
	}
	for _, port := range []any{"587", " 465 ", "abc", -1, 0, 1e12, true, nil, []any{1}} {
		cfg := parseConfig([]*pluginv1.ConfigEntry{nil, {}, mk("smtp", map[string]any{"host": 5, "port": port}), mk("silo", map[string]any{"api_key": nil})})
		if cfg.SMTP.Port <= 0 || cfg.SMTP.Port > 65535 {
			t.Errorf("port %v became %d", port, cfg.SMTP.Port)
		}
	}
}

// Recipients and state are edited from several places at once: a scheduled
// run, the admin page and a user's own page.
func TestConcurrentRunsAndEdits(t *testing.T) {
	h := newModeHarness(t, "instant")
	h.run()
	var wg sync.WaitGroup
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				h.catalog.add(fmt.Sprintf("m%d-%d", i, j), time.Now())
				h.app.runDue(ctx, "stress", true)
				_, _ = h.app.store.UpdateRecipients(ctx, func(l []Recipient) ([]Recipient, error) { return l, nil })
				h.app.dropState(ctx, "nobody")
			}
		}(i)
	}
	wg.Wait()
	h.takeSent()
	st, _ := h.state_("r1")
	ids := map[string]bool{}
	for _, id := range st.Recent {
		if ids[id] {
			t.Fatalf("item %s recorded twice, so it was probably mailed twice", id)
		}
		ids[id] = true
	}
}

func TestTokenEq(t *testing.T) {
	for _, c := range []struct {
		stored, given string
		want          bool
	}{{"abc", "abc", true}, {"abc", "abd", false}, {"abc", "ab", false}, {"", "", false}, {"", "x", false}, {"abc", "", false}} {
		if got := tokenEq(c.stored, c.given); got != c.want {
			t.Errorf("tokenEq(%q, %q) = %v", c.stored, c.given, got)
		}
	}
}

// Templates are edited by an admin and must never crash a send.
func FuzzRenderTemplates(f *testing.F) {
	for _, s := range []string{"{{title}}", "{{#items}}{{title}}{{/items}}", "{{{x}}}", "{{> p}}", "{{#a}}", "{{/a}}", "{{=<% %>=}}", "{{!c}}", "{{.}}", "{{a.b.c}}"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		s := defaultSettings()
		s.Templates.Subject, s.Templates.HTML, s.Templates.Text = src, src, src
		_ = validateTemplates(s)
		env := RenderEnv{Settings: s, ServerName: "Silo", PublicURL: "https://x", Mode: "instant", Now: time.Now(), Recipient: Recipient{Name: "P", Email: "p@x.y"}}
		_, _ = Render(context.Background(), env, sampleEntries(), nil)
	})
}

// A failing mail server must not lose items: they go out once it recovers,
// and retries back off instead of hammering it.
func TestSMTPFailureThenRecovery(t *testing.T) {
	h := newModeHarness(t, "instant")
	h.run()
	h.catalog.add("m1", time.Now())
	fail := true
	var tries int
	h.app.send = func(_ context.Context, _ SMTPConfig, msgs ...Outgoing) error {
		tries++
		if fail {
			return fmt.Errorf("421 try later")
		}
		h.mu.Lock()
		h.sent = append(h.sent, sentMail{to: msgs[0].To, subject: msgs[0].Rendered.Subject})
		h.mu.Unlock()
		return nil
	}
	for i := 0; i < 5; i++ {
		h.app.runDue(context.Background(), "test", true)
	}
	if tries != 1 {
		t.Fatalf("tried %d times in 5 back-to-back runs, want 1 (backoff)", tries)
	}
	st, _ := h.state_("r1")
	if st.Failures != 1 || st.NextAttempt.IsZero() {
		t.Fatalf("state after failure: failures=%d next=%v", st.Failures, st.NextAttempt)
	}
	// Recover: the backoff expires, and the item still goes out exactly once.
	fail = false
	st.NextAttempt = time.Now().Add(-time.Second)
	if err := h.app.store.SaveState(context.Background(), "r1", st); err != nil {
		t.Fatal(err)
	}
	h.run()
	if got := len(h.takeSent()); got != 1 {
		t.Fatalf("after recovery sent %d, want 1", got)
	}
	h.run()
	if got := len(h.takeSent()); got != 0 {
		t.Fatalf("re-sent %d on the next run", got)
	}
}

func TestLargeLibraryManyRecipients(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	h := newModeHarness(t, "instant")
	ctx := context.Background()
	var rs []Recipient
	for i := 0; i < 50; i++ {
		rs = append(rs, Recipient{ID: fmt.Sprintf("r%d", i), Email: fmt.Sprintf("u%d@example.com", i), Enabled: true,
			Mode: []string{"instant", "daily", "weekly", "instant_daily"}[i%4], Source: "admin", Verified: true, Token: fmt.Sprintf("tok%d", i), Created: time.Now()})
	}
	if err := h.app.store.SaveRecipients(ctx, rs); err != nil {
		t.Fatal(err)
	}
	h.run()
	for i := 0; i < 3000; i++ {
		h.catalog.add(fmt.Sprintf("big%d", i), time.Now())
	}
	start := time.Now()
	h.run()
	took := time.Since(start)
	sent := h.takeSent()
	t.Logf("3000 new movies, 50 recipients: %d emails in %v, item hits %d", len(sent), took, len(h.catalog.itemHits))
	if took > 20*time.Second {
		t.Errorf("run took %v", took)
	}
	for _, m := range sent {
		if len(m.subject) > 300 {
			t.Errorf("subject is %d chars", len(m.subject))
		}
	}
}

// Refreshing artwork for a ten-episode batch reads the show once, not ten
// times, and a second refresh moments later reads nothing.
func TestRefreshArtworkSharesDetails(t *testing.T) {
	h := newModeHarness(t, "instant")
	srv := httptest.NewServer(h.catalog)
	defer srv.Close()
	client := newSiloClient(srv.URL, "k", "p")
	var entries []Entry
	for i := 0; i < 10; i++ {
		entries = append(entries, Entry{Card: Card{ContentID: fmt.Sprintf("ep%d", i), Type: "episode", SeriesID: "show1"}})
	}
	h.app.refreshArtwork(context.Background(), client, entries, false, 0)
	if got := h.catalog.itemHits["show1"]; got != 1 {
		t.Errorf("show read %d times, want 1", got)
	}
	h.app.refreshArtwork(context.Background(), client, entries, false, 0)
	total := 0
	for _, n := range h.catalog.itemHits {
		total += n
	}
	if total != 11 {
		t.Errorf("%d item reads after two refreshes, want 11 (10 episodes + 1 show)", total)
	}
}

func quietSettings(start, end int) Settings {
	s := defaultSettings()
	s.Schedule.Timezone = "Europe/London"
	s.Delivery.QuietEnabled, s.Delivery.QuietStart, s.Delivery.QuietEnd = true, start, end
	return s
}

func TestQuietUntil(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	at := func(h, m int) time.Time { return time.Date(2026, 11, 6, h, m, 0, 0, loc) }
	cases := []struct {
		name       string
		s          Settings
		now        time.Time
		wantHeld   bool
		wantEndsAt string // local, "" when not held
	}{
		{"wrapping window, late evening", quietSettings(23, 7), at(23, 30), true, "2026-11-07 07:00"},
		{"wrapping window, after midnight", quietSettings(23, 7), at(2, 0), true, "2026-11-06 07:00"},
		{"wrapping window, just ended", quietSettings(23, 7), at(7, 0), false, ""},
		{"wrapping window, daytime", quietSettings(23, 7), at(14, 0), false, ""},
		{"same-day window", quietSettings(13, 15), at(14, 0), true, "2026-11-06 15:00"},
		{"same-day window, before", quietSettings(13, 15), at(12, 59), false, ""},
		{"start equals end means off", quietSettings(5, 5), at(5, 30), false, ""},
	}
	for _, c := range cases {
		got := quietUntil(c.now, c.s)
		if (!got.IsZero()) != c.wantHeld {
			t.Errorf("%s: held=%v want %v", c.name, !got.IsZero(), c.wantHeld)
			continue
		}
		if c.wantHeld && got.In(loc).Format("2006-01-02 15:04") != c.wantEndsAt {
			t.Errorf("%s: ends %s, want %s", c.name, got.In(loc).Format("2006-01-02 15:04"), c.wantEndsAt)
		}
	}
	off := quietSettings(23, 7)
	off.Delivery.QuietEnabled = false
	if !quietUntil(at(2, 0), off).IsZero() {
		t.Error("disabled quiet hours still hold")
	}
	// Clocks go forward on 2026-03-29 in London: 01:00 does not exist.
	spring := time.Date(2026, 3, 29, 0, 30, 0, 0, loc)
	if got := quietUntil(spring, quietSettings(23, 7)); got.IsZero() || got.In(loc).Hour() != 7 {
		t.Errorf("DST day: ends %v", got)
	}
}

// Instant emails wait out quiet hours, then go out together, once.
func TestQuietHoursHoldThenRelease(t *testing.T) {
	h := newModeHarness(t, "instant")
	ctx := context.Background()
	s, _ := h.app.store.Settings(ctx)
	s.Schedule.Timezone = "UTC"
	now := time.Now().UTC()
	s.Delivery.QuietEnabled = true
	s.Delivery.QuietStart = now.Hour()
	s.Delivery.QuietEnd = (now.Hour() + 2) % 24
	if err := h.app.store.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.catalog.add("night1", time.Now())
	h.catalog.add("night2", time.Now())
	h.run()
	if got := len(h.takeSent()); got != 0 {
		t.Fatalf("sent %d emails during quiet hours", got)
	}
	s.Delivery.QuietEnabled = false // the window ends
	if err := h.app.store.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	h.run()
	sent := h.takeSent()
	if len(sent) != 1 || !strings.Contains(sent[0].subject, "2 new movies") {
		t.Fatalf("after quiet hours got %+v, want one email with both movies", sent)
	}
	h.run()
	if got := len(h.takeSent()); got != 0 {
		t.Fatalf("re-sent %d", got)
	}
}

func TestHealthWarnings(t *testing.T) {
	h := newModeHarness(t, "instant")
	ctx := context.Background()
	s, _ := h.app.store.Settings(ctx)
	rs, _ := h.app.store.Recipients(ctx)

	if w := h.app.health(s, rs, nil); len(w) != 0 {
		t.Fatalf("healthy setup warned: %+v", w)
	}
	if w := h.app.health(s, nil, nil); len(w) != 1 || !strings.Contains(w[0].Text, "Nobody is signed up") {
		t.Errorf("no recipients: %+v", w)
	}
	paused := s
	paused.Delivery.Paused = true
	if w := h.app.health(paused, rs, nil); len(w) != 1 || !strings.Contains(w[0].Text, "paused") {
		t.Errorf("paused: %+v", w)
	}
	failed := []LogEntry{{Time: time.Now(), Recipient: "p@example.com", Error: "535 auth failed"}}
	if w := h.app.health(s, rs, failed); len(w) != 1 || w[0].Level != "bad" || !strings.Contains(w[0].Text, "535") {
		t.Errorf("failed send: %+v", w)
	}
	old := []LogEntry{{Time: time.Now().Add(-48 * time.Hour), Error: "old"}}
	if w := h.app.health(s, rs, old); len(w) != 0 {
		t.Errorf("a two-day-old failure still warns: %+v", w)
	}
	// A recovered send hides an older failure.
	recovered := []LogEntry{{Time: time.Now()}, {Time: time.Now().Add(-time.Hour), Error: "x"}}
	if w := h.app.health(s, rs, recovered); len(w) != 0 {
		t.Errorf("recovered: %+v", w)
	}
}

// A 401 from Silo reaches the admin page as a plain-language warning.
func TestRejectedKeyWarning(t *testing.T) {
	h := newModeHarness(t, "instant")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"title":"Unauthorized","detail":"API key is not valid"}`))
	}))
	defer srv.Close()
	h.run() // a new recipient is set up on the first run
	h.app.cfg.BaseURL = srv.URL
	h.app.runDue(context.Background(), "test", true)
	r := h.app.report()
	if r == nil || !r.KeyRejected {
		t.Fatalf("report did not record the rejected key: %+v", r)
	}
	s, _ := h.app.store.Settings(context.Background())
	rs, _ := h.app.store.Recipients(context.Background())
	w := h.app.health(s, rs, nil)
	if len(w) != 1 || !strings.Contains(w[0].Text, "rejected the plugin's API key") {
		t.Errorf("warnings: %+v", w)
	}
}
