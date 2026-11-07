package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// movieCatalog serves the newest-first movie listing the engine reads.
type movieCatalog struct {
	mu       sync.Mutex
	movies   []Card // newest first
	itemHits map[string]int
}

func (m *movieCatalog) add(id string, added time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.movies = append([]Card{{ContentID: id, Type: "movie", Title: "Movie " + id, AddedAt: added.Format(time.RFC3339Nano)}}, m.movies...)
}

func (m *movieCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/v2/catalog/items/"); ok {
		m.itemHits[id]++
		json.NewEncoder(w).Encode(Detail{Card: Card{ContentID: id, Type: "movie", Title: "Movie " + id}})
		return
	}
	if r.URL.Path != "/api/v2/catalog" {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"items": m.movies, "page": map[string]any{"has_more": false}})
}

type sentMail struct {
	to, subject string
}

type modeHarness struct {
	t       *testing.T
	app     *App
	state   *memState
	catalog *movieCatalog
	mu      sync.Mutex
	sent    []sentMail
}

func newModeHarness(t *testing.T, mode string) *modeHarness {
	t.Helper()
	h := &modeHarness{t: t, state: &memState{m: map[string][]byte{}}, catalog: &movieCatalog{itemHits: map[string]int{}}}
	srv := httptest.NewServer(h.catalog)
	t.Cleanup(srv.Close)

	h.app = newApp(hclog.NewNullLogger())
	h.app.store.backend = h.state
	h.app.libs = []*pluginv1.Library{{Id: "1", Name: "Movies", MediaType: "movie"}}
	h.app.cfg = Config{APIKey: "k", ProfileID: "p", BaseURL: srv.URL, ServerName: "Silo",
		SMTP: SMTPConfig{Host: "smtp.example.com", FromAddress: "silo@example.com"}}
	h.app.send = func(_ context.Context, _ SMTPConfig, msgs ...Outgoing) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, m := range msgs {
			h.sent = append(h.sent, sentMail{to: m.To, subject: m.Rendered.Subject})
		}
		return nil
	}

	ctx := context.Background()
	s := defaultSettings()
	s.Schedule.Timezone = "UTC"
	s.Delivery.SettleMinutes = 0
	s.Delivery.InstantGapMinutes = 0
	s.Delivery.EmbedImages = false
	if err := h.app.store.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := h.app.store.SaveRecipients(ctx, []Recipient{{ID: "r1", Name: "Pete", Email: "pete@example.com",
		Enabled: true, Mode: mode, Source: "admin", Verified: true, Token: "tok", Created: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *modeHarness) run() *RunReport {
	h.t.Helper()
	r := h.app.runDue(context.Background(), "test", true)
	if len(r.Errors) > 0 {
		h.t.Fatalf("run errors: %v", r.Errors)
	}
	return r
}

func (h *modeHarness) takeSent() []sentMail {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.sent
	h.sent = nil
	return out
}

func (h *modeHarness) state_(id string) (RecipientState, bool) {
	h.t.Helper()
	st, found, err := h.app.store.State(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return st, found
}

// makeDigestDue moves a delivery's last run back past the daily hour.
func (h *modeHarness) makeDigestDue(id string) {
	h.t.Helper()
	st, _ := h.state_(id)
	st.LastRun = time.Now().Add(-25 * time.Hour)
	if err := h.app.store.SaveState(context.Background(), id, st); err != nil {
		h.t.Fatal(err)
	}
}

func TestDeliveries(t *testing.T) {
	for mode, want := range map[string]string{"instant": "instant", "daily": "daily", "weekly": "weekly", "instant_daily": "instant,daily"} {
		if got := strings.Join(deliveries(mode), ","); got != want {
			t.Errorf("deliveries(%q) = %q, want %q", mode, got, want)
		}
	}
	r := Recipient{ID: "x", Mode: "instant_daily"}
	if deliveryStateID(r, "instant") != "x" || deliveryStateID(r, "daily") != "x:daily" {
		t.Error("combined mode state ids")
	}
	r.Mode = "daily"
	if deliveryStateID(r, "daily") != "x" {
		t.Error("plain daily should use the recipient's own state")
	}
	for in, want := range map[string]string{"instant_daily": "instant_daily", "daily": "daily", "weekly": "weekly", "": "instant", "bogus": "instant"} {
		if got := cleanMode(in); got != want {
			t.Errorf("cleanMode(%q) = %q, want %q", in, got, want)
		}
	}
	s := defaultSettings()
	now := time.Now()
	if !nextDue("instant_daily", s, now).Equal(nextDue("daily", s, now)) {
		t.Error("combined mode should show the daily digest as next due")
	}
}

func TestInstantPlusDailyDigest(t *testing.T) {
	h := newModeHarness(t, "instant_daily")

	// First run starts both deliveries from now; nothing is sent.
	h.run()
	if sent := h.takeSent(); len(sent) != 0 {
		t.Fatalf("baseline sent %v", sent)
	}
	if _, ok := h.state_("r1"); !ok {
		t.Fatal("instant state not created")
	}
	if _, ok := h.state_("r1:daily"); !ok {
		t.Fatal("digest state not created")
	}

	// A new movie goes out at once as an instant alert; the digest isn't due.
	h.catalog.add("m1", time.Now().Add(-time.Second))
	h.run()
	sent := h.takeSent()
	if len(sent) != 1 || !strings.Contains(sent[0].subject, "Movie m1") || strings.Contains(sent[0].subject, "digest") {
		t.Fatalf("instant: %v", sent)
	}

	// A second movie arrives; then the daily hour passes. The instant alert
	// covers the new movie, and the digest recaps both.
	h.catalog.add("m2", time.Now().Add(-time.Second))
	h.makeDigestDue("r1:daily")
	r := h.run()
	sent = h.takeSent()
	if r.Sent != 2 || len(sent) != 2 {
		t.Fatalf("sent %d: %v", r.Sent, sent)
	}
	var instant, digest string
	for _, m := range sent {
		if strings.Contains(m.subject, "daily digest") {
			digest = m.subject
		} else {
			instant = m.subject
		}
	}
	if !strings.Contains(instant, "Movie m2") {
		t.Errorf("instant subject = %q", instant)
	}
	if !strings.Contains(digest, "2 new movies") {
		t.Errorf("digest subject = %q", digest)
	}

	// Nothing more is due until something new arrives or the next daily hour.
	h.run()
	if sent := h.takeSent(); len(sent) != 0 {
		t.Fatalf("repeat run sent %v", sent)
	}

	// Each delivery is logged under its own mode.
	logs, _ := h.app.store.Log(context.Background())
	modes := map[string]int{}
	for _, l := range logs {
		modes[l.Mode]++
	}
	if modes["instant"] != 2 || modes["daily"] != 1 {
		t.Fatalf("log modes = %v", modes)
	}

	// The instant rate limit only holds back instant alerts, never the digest.
	s, _ := h.app.store.Settings(context.Background())
	s.Delivery.InstantGapMinutes = 60
	h.app.store.SaveSettings(context.Background(), s)
	h.catalog.add("m3", time.Now().Add(-time.Second))
	h.makeDigestDue("r1:daily")
	h.run()
	sent = h.takeSent()
	if len(sent) != 1 || !strings.Contains(sent[0].subject, "daily digest: 1 new movie") {
		t.Fatalf("with the instant gap: %v", sent)
	}
}

func TestDigestStartsAfreshAfterSwitchingBack(t *testing.T) {
	h := newModeHarness(t, "instant_daily")
	ctx := context.Background()
	h.run()

	// Switch to instant only: the digest state is dropped.
	r := Recipient{ID: "r1", Name: "Pete", Email: "pete@example.com", Enabled: true, Mode: "instant"}
	if _, err := h.app.saveAdminRecipient(ctx, r); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.state_("r1:daily"); !st.Watermark.IsZero() {
		t.Fatal("digest state kept after leaving the combined mode")
	}

	// A backlog builds up while only instant alerts are on. (The digest
	// looks back two hours for late-stamped items, so make it older.)
	h.catalog.add("old", time.Now().Add(-3*time.Hour))
	h.run()
	h.takeSent()

	// Switching back doesn't send a digest of the backlog: the digest starts
	// from now, and its first email is at the next daily hour.
	r.Mode = "instant_daily"
	if _, err := h.app.saveAdminRecipient(ctx, r); err != nil {
		t.Fatal(err)
	}
	h.run()
	if sent := h.takeSent(); len(sent) != 0 {
		t.Fatalf("switching back sent %v", sent)
	}
	h.makeDigestDue("r1:daily")
	h.run()
	if sent := h.takeSent(); len(sent) != 0 {
		t.Fatalf("first digest after switching back included the backlog: %v", sent)
	}
}

func TestCombinedModeAdmin(t *testing.T) {
	h := newModeHarness(t, "instant_daily")
	ctx := context.Background()
	h.run()
	h.catalog.add("m1", time.Now().Add(-time.Second))
	h.run()
	h.takeSent()

	// The admin view shows the last email and the next digest.
	var view struct {
		Recipients []struct {
			Mode     string     `json:"mode"`
			LastSent *time.Time `json:"last_sent"`
			NextDue  *time.Time `json:"next_due"`
		} `json:"recipients"`
	}
	resp := h.app.adminState(ctx)
	if err := json.Unmarshal(resp.Body, &view); err != nil || len(view.Recipients) != 1 {
		t.Fatalf("state: %s", resp.Body)
	}
	v := view.Recipients[0]
	if v.Mode != "instant_daily" || v.LastSent == nil || v.NextDue == nil {
		t.Fatalf("view = %+v", v)
	}

	// Resend 24h rewinds both deliveries.
	if r := h.app.adminAPI(ctx, request{method: "POST", body: []byte(`{"hours":24}`)}, "recipients/r1/rewind"); r.StatusCode != 200 {
		t.Fatalf("rewind: %d %s", r.StatusCode, r.Body)
	}
	h.run()
	sent := h.takeSent()
	if len(sent) != 2 {
		t.Fatalf("after rewind sent %v", sent)
	}

	// Removing the recipient removes both states.
	if r := h.app.adminAPI(ctx, request{method: "DELETE"}, "recipients/r1"); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	for _, id := range []string{"r1", "r1:daily"} {
		if st, _ := h.state_(id); !st.Watermark.IsZero() {
			t.Errorf("%s state left behind", id)
		}
	}
}

func TestManageFormOffersCombinedMode(t *testing.T) {
	s := defaultSettings()
	form := manageForm("tok", Recipient{Mode: "instant_daily", Enabled: true}, s)
	if !strings.Contains(form, `value="instant_daily" checked`) {
		t.Fatalf("combined mode not offered or not selected:\n%s", form)
	}
	if strings.Count(form, " checked") != 4 { // enabled, mode, two types
		t.Fatalf("unexpected selections:\n%s", form)
	}
}

func TestMaxAgeStopsStaleBacklog(t *testing.T) {
	// Today's incident: the recipient's state was far behind, so a whole
	// back catalogue looked new. The max age keeps it to genuinely new items.
	h := newModeHarness(t, "instant")
	ctx := context.Background()
	if err := h.app.store.SaveState(ctx, "r1", RecipientState{Watermark: time.Now().AddDate(0, 0, -30), LastRun: time.Now().AddDate(0, 0, -30)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		h.catalog.add("old"+strconv.Itoa(i), time.Now().AddDate(0, 0, -10).Add(time.Duration(i)*time.Minute))
	}
	h.catalog.add("new", time.Now().Add(-time.Hour))
	h.run()
	sent := h.takeSent()
	if len(sent) != 1 || !strings.Contains(sent[0].subject, "Movie new") {
		t.Fatalf("sent %v, want only the new movie", sent)
	}
}

func TestRecipientMaxAgeOverridesDefault(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		hours int
		want  string
	}{
		{0, "2 new movies"},   // server default, 1 week: both
		{48, "Movie recent"},  // 48 hours: only the recent one
		{720, "2 new movies"}, // 30 days: both
	} {
		h := newModeHarness(t, "instant")
		list, _ := h.app.store.Recipients(ctx)
		list[0].MaxAgeHours = c.hours
		h.app.store.SaveRecipients(ctx, list)
		h.app.store.SaveState(ctx, "r1", RecipientState{Watermark: time.Now().AddDate(0, 0, -10), LastRun: time.Now()})
		h.catalog.add("older", time.Now().Add(-72*time.Hour))
		h.catalog.add("recent", time.Now().Add(-time.Hour))
		h.run()
		sent := h.takeSent()
		if len(sent) != 1 || !strings.Contains(sent[0].subject, c.want) {
			t.Errorf("max age %d: sent %v, want %q", c.hours, sent, c.want)
		}
	}
}

func TestMaxAgeSettingsAndChoices(t *testing.T) {
	if defaultSettings().Delivery.MaxAgeHours != 168 {
		t.Fatal("default should be one week")
	}
	for in, want := range map[int]int{0: 168, -5: 168, 48: 48, 5000: maxAgeLimit} {
		s := defaultSettings()
		s.Delivery.MaxAgeHours = in
		s.normalize()
		if s.Delivery.MaxAgeHours != want {
			t.Errorf("normalize(%d) = %d, want %d", in, s.Delivery.MaxAgeHours, want)
		}
	}
	for in, want := range map[int]int{24: 24, 48: 48, 720: 720, 0: 0, 5: 0, 100000: 0} {
		if got := cleanMaxAge(in); got != want {
			t.Errorf("cleanMaxAge(%d) = %d, want %d", in, got, want)
		}
	}
	for h, want := range map[int]string{24: "24 hours", 48: "48 hours", 72: "3 days", 168: "1 week", 336: "2 weeks", 720: "30 days"} {
		if got := maxAgeLabel(h); got != want {
			t.Errorf("maxAgeLabel(%d) = %q, want %q", h, got, want)
		}
	}
	opts := maxAgeOptions(defaultSettings())
	if opts[0].Hours != 0 || opts[0].Label != "Server default (1 week)" || len(opts) != len(maxAgeChoices)+1 {
		t.Fatalf("options = %+v", opts)
	}
}

func TestManagePageSetsMaxAge(t *testing.T) {
	h := newModeHarness(t, "instant")
	ctx := context.Background()
	page := h.app.publicRoute(ctx, request{method: "GET", query: map[string]any{"t": "tok"}}, "manage")
	if !strings.Contains(string(page.Body), `<option value="0" selected>Server default (1 week)</option>`) {
		t.Fatalf("manage page doesn't offer the max age:\n%s", page.Body)
	}
	body := "t=tok&enabled=on&mode=instant&types=episode&types=movie&max_age=48"
	if r := h.app.publicRoute(ctx, request{method: "POST", body: []byte(body)}, "manage"); r.StatusCode != 200 {
		t.Fatalf("save: %d %s", r.StatusCode, r.Body)
	}
	list, _ := h.app.store.Recipients(ctx)
	if list[0].MaxAgeHours != 48 {
		t.Fatalf("max age = %d", list[0].MaxAgeHours)
	}
	// A value that isn't offered falls back to the server default.
	body = strings.Replace(body, "max_age=48", "max_age=9999", 1)
	h.app.publicRoute(ctx, request{method: "POST", body: []byte(body)}, "manage")
	list, _ = h.app.store.Recipients(ctx)
	if list[0].MaxAgeHours != 0 {
		t.Fatalf("unoffered value saved as %d", list[0].MaxAgeHours)
	}
}

// A run whose context is cancelled while the email is being sent (an admin
// request that timed out) must still record the send, or the next run mails
// the same items again.
func TestCancelledRunStillRecordsSend(t *testing.T) {
	h := newModeHarness(t, "instant")
	h.run() // a new recipient starts from now
	h.catalog.add("m1", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	h.app.send = func(_ context.Context, _ SMTPConfig, msgs ...Outgoing) error {
		h.mu.Lock()
		h.sent = append(h.sent, sentMail{to: msgs[0].To, subject: msgs[0].Rendered.Subject})
		h.mu.Unlock()
		cancel() // the request goes away just after the mail is accepted
		return nil
	}
	h.app.runDue(ctx, "test", true)
	if got := len(h.takeSent()); got != 1 {
		t.Fatalf("first run sent %d emails, want 1", got)
	}
	st, _ := h.state_("r1")
	if st.LastSent.IsZero() {
		t.Fatal("send was not recorded after the context was cancelled")
	}
	h.run()
	if got := len(h.takeSent()); got != 0 {
		t.Fatalf("second run re-sent %d emails, want 0", got)
	}
}

func TestRetryScheduleAndLastError(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if got := retryDelay(i + 1); got != w {
			t.Fatalf("retryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	h := newModeHarness(t, "instant")
	h.run()
	h.catalog.add("m1", time.Now())
	h.app.send = func(context.Context, SMTPConfig, ...Outgoing) error { return errors.New("smtp down") }
	r := h.app.runDue(context.Background(), "test", true)
	if len(r.Errors) == 0 {
		t.Fatal("expected a send error")
	}
	st, _ := h.state_("r1")
	if st.Failures != 1 || st.LastError == "" || st.Claim != nil {
		t.Fatalf("state after failure: %+v", st)
	}
	if d := time.Until(st.NextAttempt); d < 50*time.Second || d > 70*time.Second {
		t.Fatalf("first retry in %v, want about 1m", d)
	}
}

func TestBurstSendsOneCompactEmailWithoutCap(t *testing.T) {
	h := newModeHarness(t, "instant")
	h.run()
	s, _ := h.app.store.Settings(context.Background())
	s.Delivery.BurstThreshold = 5
	s.Delivery.MaxItems = 3
	h.app.store.SaveSettings(context.Background(), s)
	for i := 0; i < 12; i++ {
		h.catalog.add("m"+strconv.Itoa(i), time.Now())
	}
	h.run()
	if got := len(h.takeSent()); got != 1 {
		t.Fatalf("sent %d emails, want 1", got)
	}
	logs, _ := h.app.store.Log(context.Background())
	last := logs[len(logs)-1]
	if !last.Burst || last.Items != 12 || len(last.ItemIDs) != 12 {
		t.Fatalf("log entry: burst=%v items=%d ids=%d", last.Burst, last.Items, len(last.ItemIDs))
	}
	// Below the threshold the old cap applies.
	for i := 20; i < 24; i++ {
		h.catalog.add("m"+strconv.Itoa(i), time.Now())
	}
	h.run()
	logs, _ = h.app.store.Log(context.Background())
	if logs[len(logs)-1].Burst {
		t.Fatal("4 items should not be a burst")
	}
}

func TestClaimStopsSecondProcessSending(t *testing.T) {
	claimSettle = 0
	h := newModeHarness(t, "instant")
	h.run()
	h.catalog.add("m1", time.Now())
	// Another process holds a fresh claim.
	st, _ := h.state_("r1")
	st.Claim = &Claim{Nonce: "other", PID: 99, At: time.Now()}
	h.app.store.SaveState(context.Background(), "r1", st)
	h.run()
	if got := len(h.takeSent()); got != 0 {
		t.Fatalf("sent %d while another process held the claim", got)
	}
	// A stale claim (crashed process) does not block.
	st.Claim = &Claim{Nonce: "other", PID: 99, At: time.Now().Add(-claimTTL - time.Minute)}
	h.app.store.SaveState(context.Background(), "r1", st)
	h.run()
	if got := len(h.takeSent()); got != 1 {
		t.Fatalf("sent %d after stale claim, want 1", got)
	}
}

func TestClaimDropsItemsAnotherProcessSent(t *testing.T) {
	claimSettle = 0
	h := newModeHarness(t, "instant")
	h.run()
	h.catalog.add("m1", time.Now())
	// Between picking and sending, another process records m1 as sent.
	orig := h.app.send
	_ = orig
	st, _ := h.state_("r1")
	st.Recent = append(st.Recent, "m1")
	h.app.store.SaveState(context.Background(), "r1", st)
	h.run()
	if got := len(h.takeSent()); got != 0 {
		t.Fatalf("re-sent an item already in the ledger: %d", got)
	}
}

// Two plugin processes share one state store. However they interleave, an
// item must be emailed once.
func TestTwoProcessesNeverDoubleSend(t *testing.T) {
	claimSettle = 50 * time.Millisecond
	defer func() { claimSettle = 0 }()
	for round := 0; round < 5; round++ {
		h := newModeHarness(t, "instant")
		h.run()
		second := newApp(hclog.NewNullLogger())
		second.store.backend = h.state
		second.libs = h.app.libs
		second.cfg = h.app.cfg
		second.send = h.app.send
		h.catalog.add("m1", time.Now())
		var wg sync.WaitGroup
		for _, app := range []*App{h.app, second} {
			wg.Add(1)
			go func(app *App) {
				defer wg.Done()
				app.runDue(context.Background(), "test", true)
			}(app)
		}
		wg.Wait()
		if got := len(h.takeSent()); got != 1 {
			t.Fatalf("round %d: sent %d emails for one item, want 1", round, got)
		}
	}
}
