package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

const (
	// overlap re-reads items slightly older than a recipient's watermark, so an
	// item whose added_at was stamped before a scan committed is not missed.
	// The recipient's Recent list stops it being sent twice.
	overlap = 2 * time.Hour
	// pollEvery is how often the catalog is checked without a scan event. It
	// catches items that appear without one, such as a late match.
	pollEvery = time.Minute
	// runTimeout bounds one background run, including every email it sends.
	runTimeout = 10 * time.Minute
	// persistTimeout bounds saving state and the log after a send. It runs on
	// a context detached from the caller's, so a cancelled request can't lose
	// the record of an email that already went out.
	persistTimeout = 30 * time.Second
	// dailySuffix marks the digest half of an instant_daily recipient's state.
	dailySuffix = ":daily"
	// claimTTL is how long another process's send claim blocks a recipient
	// before it is treated as left behind by a crash.
	claimTTL  = runTimeout
	tickEvery = 30 * time.Second
	// eventBatch is the short wait after a scan event, so items the same
	// scan adds in quick succession go out in one email.
	eventBatch = 5 * time.Second
	// detailTTL bounds how long series and movie details are reused. Their
	// artwork URLs are presigned and short-lived, so keep this brief.
	detailTTL = 10 * time.Minute
	// freshWindow is how recent a cached detail must be to count as fresh when
	// artwork is refreshed just before sending.
	freshWindow = 2 * time.Minute
)

// retrySchedule is the wait after each consecutive failed send: 1 minute,
// 5, 30, 2 hours, then every 6 hours until it works or the items age out.
var retrySchedule = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

func retryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > len(retrySchedule) {
		failures = len(retrySchedule)
	}
	return retrySchedule[failures-1]
}

// claimSettle is the pause between writing a send claim and reading it back,
// long enough for a competing process's write to land first. Tests set it to 0.
var claimSettle = 150 * time.Millisecond

// claim takes the right to send picked to one recipient. It re-reads the
// recipient's state, drops items another process already sent, and writes a
// claim that it reads back to confirm nobody else holds the recipient. It
// returns the fresh state and the items still to send; ok is false when the
// send should be skipped (another process is sending, or nothing is left).
func (a *App) claim(ctx context.Context, j job, picked []Entry) (RecipientState, []Entry, bool, error) {
	pctx, done := persistCtx(ctx)
	defer done()
	st, found, err := a.store.State(pctx, j.stateID)
	if err != nil {
		return j.st, nil, false, err
	}
	if !found {
		st = j.st
	}
	if c := st.Claim; c != nil && time.Since(c.At) < claimTTL {
		return st, nil, false, nil
	}
	sent := setOf(st.Recent)
	todo := make([]Entry, 0, len(picked))
	for _, e := range picked {
		if !sent[e.Card.ContentID] {
			todo = append(todo, e)
		}
	}
	if len(todo) == 0 {
		return st, nil, false, nil
	}
	mine := &Claim{Nonce: newID(), PID: os.Getpid(), At: time.Now()}
	st.Claim = mine
	if err := a.store.SaveState(pctx, j.stateID, st); err != nil {
		return st, nil, false, err
	}
	if claimSettle > 0 {
		time.Sleep(claimSettle)
	}
	back, _, err := a.store.State(pctx, j.stateID)
	if err != nil {
		return st, nil, false, err
	}
	if back.Claim == nil || back.Claim.Nonce != mine.Nonce {
		return back, nil, false, nil
	}
	return back, todo, true, nil
}

type App struct {
	log   hclog.Logger
	store *Store

	mu  sync.RWMutex
	cfg Config

	runMu sync.Mutex

	loopOnce   sync.Once
	triggerMu  sync.Mutex
	eventTimer *time.Timer
	lastPoll   time.Time

	reportMu   sync.Mutex
	lastReport *RunReport

	detailMu sync.Mutex
	details  map[string]cachedDetail

	profileMu sync.Mutex
	profileID string
	profileAt time.Time

	// libs replaces the host's library list in tests.
	libs []*pluginv1.Library
	// send replaces sendMail in tests.
	send func(context.Context, SMTPConfig, ...Outgoing) error

	bg bgCache
}

type cachedDetail struct {
	d  *Detail
	at time.Time
}

type RunReport struct {
	Time       time.Time `json:"time"`
	Reason     string    `json:"reason"`
	Recipients int       `json:"recipients_due"`
	Sent       int       `json:"emails_sent"`
	Items      int       `json:"items_found"`
	Errors     []string  `json:"errors,omitempty"`
	Skipped    string    `json:"skipped,omitempty"`
	// KeyRejected is set when Silo answered the plugin's API key with 401 or
	// 403, so the admin page can say so.
	KeyRejected bool `json:"key_rejected,omitempty"`
}

// isAuthError reports whether err is Silo refusing the plugin's API key.
func isAuthError(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

func newApp(log hclog.Logger) *App {
	return &App{log: log, store: &Store{}, details: map[string]cachedDetail{}}
}

func (a *App) configure(entries []*pluginv1.ConfigEntry) {
	cfg := parseConfig(entries)
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	a.ensureLoop()
}

func (a *App) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// ensureLoop starts the background ticker once per process. Silo starts the
// plugin for the startup task, an event, or a page visit, and keeps it running.
func (a *App) ensureLoop() {
	a.loopOnce.Do(func() {
		go func() {
			t := time.NewTicker(tickEvery)
			defer t.Stop()
			for range t.C {
				a.tick()
			}
		}()
	})
}

// trigger runs a catalog check as soon as the settle delay (zero by default
// for instant alerts) and a short batching window have passed. A run already
// scheduled is left alone, so a long scan can't keep pushing it back; events
// that arrive while it runs schedule the next one.
func (a *App) trigger(settle time.Duration) {
	a.triggerMu.Lock()
	defer a.triggerMu.Unlock()
	if a.eventTimer != nil {
		return
	}
	a.eventTimer = time.AfterFunc(settle+eventBatch, func() {
		a.triggerMu.Lock()
		a.eventTimer = nil
		a.triggerMu.Unlock()
		a.run("new media event", true)
	})
}

func (a *App) tick() {
	a.triggerMu.Lock()
	pollDue := time.Since(a.lastPoll) >= pollEvery
	a.triggerMu.Unlock()
	reason := "schedule"
	if pollDue {
		reason = "periodic check"
	}
	// Digests are checked every tick; the catalog is only read when someone
	// is due, so a quiet tick costs a few state reads.
	a.run(reason, pollDue)
}

// startRun begins a catalog run in the background and reports whether it
// started. It returns false when a run is already in progress.
func (a *App) startRun(reason string) bool {
	if !a.runMu.TryLock() {
		return false
	}
	a.runMu.Unlock()
	go a.run(reason, true)
	return true
}

func (a *App) run(reason string, readCatalog bool) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	a.runDue(ctx, reason, readCatalog)
}

// endpoints resolves the URLs the plugin needs from config and host info.
type endpoints struct {
	internal   string // Silo API base reachable from the plugin
	public     string // public Silo URL for links
	pluginBase string // public URL of this installation's pages
}

func (a *App) endpoints(ctx context.Context) endpoints {
	cfg := a.config()
	var ep endpoints
	host := sdkruntime.Host()
	if host != nil {
		if info, err := host.GetHostInfo(ctx); err == nil {
			ep.public = strings.TrimRight(info.PublicBaseURL, "/")
			ep.pluginBase = strings.TrimRight(info.PluginProxyBaseURL, "/")
			ep.internal = loopbackURL(info.InternalBaseURL)
		} else {
			a.log.Warn("host info unavailable", "error", err)
		}
	}
	if cfg.PublicURL != "" {
		if ep.pluginBase != "" && ep.public != "" && strings.HasPrefix(ep.pluginBase, ep.public) {
			ep.pluginBase = cfg.PublicURL + strings.TrimPrefix(ep.pluginBase, ep.public)
		}
		ep.public = cfg.PublicURL
	}
	if cfg.BaseURL != "" {
		ep.internal = cfg.BaseURL
	}
	if ep.internal == "" {
		ep.internal = "http://127.0.0.1:8080"
	}
	return ep
}

// loopbackURL turns a listener such as http://:8080 or http://0.0.0.0:8080
// into an address the plugin can dial.
func loopbackURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return strings.TrimRight(raw, "/")
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	}
	return strings.TrimRight(u.String(), "/")
}

// client returns a catalog client acting as the configured profile, or as
// the API key owner's primary profile.
func (a *App) client(ctx context.Context, ep endpoints) *SiloClient {
	cfg := a.config()
	c := newSiloClient(ep.internal, cfg.APIKey, cfg.ProfileID)
	if cfg.ProfileID != "" || cfg.APIKey == "" {
		return c
	}
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	if a.profileID == "" || time.Since(a.profileAt) > time.Hour {
		id, err := c.PrimaryProfile(ctx)
		if err != nil {
			a.log.Warn("resolve profile", "error", err)
		} else {
			a.profileID, a.profileAt = id, time.Now()
		}
	}
	c.profile = a.profileID
	return c
}

type job struct {
	r     Recipient
	st    RecipientState
	since time.Time
	// mode is the delivery this job is for: the recipient's mode, or one
	// half of a combined mode (see deliveries).
	mode    string
	stateID string
}

// deliveries splits a recipient's mode into the emails they get. The
// combined instant_daily mode gets instant alerts and a daily digest, each
// tracked in its own state so one never holds back the other; the digest
// recaps the day, including items an instant alert already covered.
func deliveries(mode string) []string {
	if mode == "instant_daily" {
		return []string{"instant", "daily"}
	}
	return []string{mode}
}

// deliveryStateID is the state id for one of a recipient's deliveries. The
// first delivery uses the recipient's own id, so switching between instant
// and instant_daily keeps the instant history.
// dailyStateID is the state id of the digest half of recipient id.
func dailyStateID(id string) string { return id + dailySuffix }

// persistCtx returns a context for saving state that outlives ctx's
// cancellation.
func persistCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
}

func deliveryStateID(r Recipient, mode string) string {
	if r.Mode == "instant_daily" && mode == "daily" {
		return dailyStateID(r.ID)
	}
	return r.ID
}

// runDue sends every email that is due. readCatalog=false limits the run to
// digests whose time has come.
func (a *App) runDue(ctx context.Context, reason string, readCatalog bool) *RunReport {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	report := &RunReport{Time: time.Now(), Reason: reason}
	defer a.saveReport(report, readCatalog)

	cfg := a.config()
	if p := cfg.problems(); len(p) > 0 {
		report.Skipped = strings.Join(p, " ")
		return report
	}
	settings, err := a.store.Settings(ctx)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}
	if settings.Delivery.Paused {
		report.Skipped = "Sending is paused."
		return report
	}
	recipients, err := a.store.Recipients(ctx)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		return report
	}

	ep := a.endpoints(ctx)
	client := a.client(ctx, ep)
	if readCatalog {
		// Record new episodes every check, whoever is due, so digests see them.
		if _, err := a.detectTV(ctx, client); err != nil {
			report.Errors = append(report.Errors, err.Error())
			report.KeyRejected = report.KeyRejected || isAuthError(err)
		}
	}

	now := time.Now()
	cutoff := now.Add(-time.Duration(settings.Delivery.SettleMinutes) * time.Minute)
	var jobs []job
	for _, r := range recipients {
		if !r.active(settings) {
			continue
		}
		for _, mode := range deliveries(r.Mode) {
			id := deliveryStateID(r, mode)
			st, found, err := a.store.State(ctx, id)
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			if !found || st.Watermark.IsZero() {
				// A new recipient starts from now rather than receiving the
				// whole back catalogue.
				st = RecipientState{Watermark: cutoff, LastRun: now}
				if err := a.store.SaveState(ctx, id, st); err != nil {
					report.Errors = append(report.Errors, err.Error())
				}
				continue
			}
			if !st.NextAttempt.IsZero() && now.Before(st.NextAttempt) {
				continue
			}
			if !isDue(mode, st.LastRun, now, settings) {
				continue
			}
			if mode == "instant" && !readCatalog {
				continue
			}
			if mode == "instant" && !instantHeldUntil(st, settings, now).IsZero() {
				// Rate limited: new items wait and go out together later.
				continue
			}
			// Never reach back past the recipient's idea of "new", even if
			// the watermark is old (a long outage, a rewind, a stale state).
			since := maxTime(st.Watermark.Add(-overlap), now.Add(-r.maxAge(settings)))
			jobs = append(jobs, job{r: r, st: st, since: since, mode: mode, stateID: id})
		}
	}
	report.Recipients = len(jobs)
	if len(jobs) == 0 {
		return report
	}

	since := jobs[0].since
	for _, j := range jobs[1:] {
		if j.since.Before(since) {
			since = j.since
		}
	}
	entries, err := a.collect(ctx, client, since, settings.Delivery.MaxItems*4)
	if err != nil {
		report.Errors = append(report.Errors, "reading the catalog: "+err.Error())
		report.KeyRejected = report.KeyRejected || isAuthError(err)
		if len(entries) == 0 {
			return report
		}
	}
	report.Items = len(entries)

	var logs []LogEntry
	for _, j := range jobs {
		picked := pickFor(j, entries, cutoff)
		st := j.st
		if len(picked) == 0 {
			st.LastRun = now
			st.Watermark = maxTime(st.Watermark, cutoff)
			if err := a.store.SaveState(ctx, j.stateID, st); err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
			continue
		}
		fresh, todo, ok, err := a.claim(ctx, j, picked)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", j.r.Email, err))
			continue
		}
		if !ok {
			continue
		}
		st = fresh
		picked = todo
		burst := settings.Delivery.BurstThreshold > 0 && len(picked) > settings.Delivery.BurstThreshold
		more := 0
		if !burst && len(picked) > settings.Delivery.MaxItems {
			more = len(picked) - settings.Delivery.MaxItems
			picked = picked[:settings.Delivery.MaxItems]
		}
		env := a.env(settings, cfg, ep, j.r, j.mode, now)
		env.MoreCount = more
		env.Compact = burst
		subject, err := a.deliver(ctx, cfg, client, env, picked)
		ids := make([]string, 0, len(picked))
		for _, e := range picked {
			ids = append(ids, e.Card.ContentID)
		}
		entry := LogEntry{Time: time.Now(), Started: now, PID: os.Getpid(), ItemIDs: ids,
			Recipient: j.r.Email, Mode: j.mode, Items: len(picked) + more, Subject: subject, Burst: burst}
		if err != nil {
			entry.Error = err.Error()
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", j.r.Email, err))
			st.Failures++
			st.NextAttempt = time.Now().Add(retryDelay(st.Failures))
			st.LastError, st.LastErrorAt = err.Error(), time.Now()
			st.Claim = nil
		} else {
			report.Sent++
			st.Failures = 0
			st.NextAttempt = time.Time{}
			st.LastError, st.LastErrorAt = "", time.Time{}
			st.Claim = nil
			st.LastSent = now
			st.LastRun = now
			if j.mode == "instant" {
				st.LastInstant = now
				if day := now.In(settings.location()).Format("2006-01-02"); st.Day == day {
					st.DayCount++
				} else {
					st.Day, st.DayCount = day, 1
				}
			}
			st.Watermark = maxTime(st.Watermark, cutoff)
			for _, e := range picked {
				st.Recent = append(st.Recent, e.Card.ContentID)
			}
		}
		// The send already happened, so losing this record means a repeat:
		// try a few times before giving up.
		for try := 0; try < 3; try++ {
			pctx, done := persistCtx(ctx)
			err := a.store.SaveState(pctx, j.stateID, st)
			done()
			if err == nil {
				break
			}
			if try == 2 {
				report.Errors = append(report.Errors, err.Error())
			} else {
				time.Sleep(time.Second)
			}
		}
		logs = append(logs, entry)
	}
	if len(logs) > 0 {
		pctx, done := persistCtx(ctx)
		if err := a.store.AppendLog(pctx, logs...); err != nil {
			a.log.Warn("append log", "error", err)
		}
		done()
	}
	return report
}

func (a *App) saveReport(r *RunReport, significant bool) {
	if !significant && r.Recipients == 0 && len(r.Errors) == 0 {
		return
	}
	if len(r.Errors) > 0 {
		a.log.Warn("run finished with errors", "reason", r.Reason, "errors", strings.Join(r.Errors, "; "))
	}
	a.reportMu.Lock()
	a.lastReport = r
	a.reportMu.Unlock()
	if significant {
		a.triggerMu.Lock()
		a.lastPoll = r.Time
		a.triggerMu.Unlock()
	}
}

func (a *App) report() *RunReport {
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	return a.lastReport
}

// maxAge is how recently an item must have been added to be emailed to r.
func (r Recipient) maxAge(s Settings) time.Duration {
	h := r.MaxAgeHours
	if h <= 0 {
		h = s.Delivery.MaxAgeHours
	}
	return time.Duration(h) * time.Hour
}

func (r Recipient) active(s Settings) bool {
	if !r.Enabled || r.Email == "" {
		return false
	}
	if r.Source == "user" && s.SelfService.RequireVerification && !r.Verified {
		return false
	}
	if r.Source == "user" && !s.SelfService.Enabled {
		return false
	}
	return true
}

// instantHeldUntil returns when an instant recipient may next be emailed, or
// zero when they may be emailed now.
func instantHeldUntil(st RecipientState, s Settings, now time.Time) time.Time {
	var until time.Time
	if gap := time.Duration(s.Delivery.InstantGapMinutes) * time.Minute; gap > 0 && !st.LastInstant.IsZero() {
		if next := st.LastInstant.Add(gap); now.Before(next) {
			until = next
		}
	}
	if limit := s.Delivery.InstantDailyCap; limit > 0 {
		local := now.In(s.location())
		if st.Day == local.Format("2006-01-02") && st.DayCount >= limit {
			midnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, local.Location())
			if midnight.After(until) {
				until = midnight
			}
		}
	}
	if q := quietUntil(now, s); q.After(until) {
		until = q
	}
	return until
}

// quietUntil returns when the quiet-hours window containing now ends, or zero
// when quiet hours are off or now is outside the window.
func quietUntil(now time.Time, s Settings) time.Time {
	d := s.Delivery
	if !d.QuietEnabled || d.QuietStart == d.QuietEnd {
		return time.Time{}
	}
	loc := s.location()
	h := now.In(loc).Hour()
	inside := h >= d.QuietStart && h < d.QuietEnd
	if d.QuietStart > d.QuietEnd { // the window wraps past midnight
		inside = h >= d.QuietStart || h < d.QuietEnd
	}
	if !inside {
		return time.Time{}
	}
	return nextBoundary(now, loc, -1, d.QuietEnd)
}

// isDue reports whether a recipient's next email may go out now.
func isDue(mode string, lastRun, now time.Time, s Settings) bool {
	switch mode {
	case "daily":
		return lastRun.Before(lastBoundary(now, s.location(), -1, s.Schedule.DailyHour))
	case "weekly":
		return lastRun.Before(lastBoundary(now, s.location(), s.Schedule.WeeklyDay, s.Schedule.WeeklyHour))
	default:
		return true
	}
}

// lastBoundary returns the most recent time at or before now that falls on
// hour (and on weekday, unless weekday is -1).
func lastBoundary(now time.Time, loc *time.Location, weekday, hour int) time.Time {
	local := now.In(loc)
	b := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, loc)
	for i := 0; i < 8; i++ {
		if !b.After(local) && (weekday < 0 || int(b.Weekday()) == weekday) {
			return b
		}
		b = b.AddDate(0, 0, -1)
	}
	return b
}

func nextBoundary(now time.Time, loc *time.Location, weekday, hour int) time.Time {
	local := now.In(loc)
	b := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, loc)
	for i := 0; i < 8; i++ {
		if b.After(local) && (weekday < 0 || int(b.Weekday()) == weekday) {
			return b
		}
		b = b.AddDate(0, 0, 1)
	}
	return b
}

func pickFor(j job, entries []Entry, cutoff time.Time) []Entry {
	recent := make(map[string]bool, len(j.st.Recent))
	for _, id := range j.st.Recent {
		recent[id] = true
	}
	types := setOf(j.r.Types)
	libs := setOf(j.r.Libraries)
	var out []Entry
	for _, e := range entries {
		added := e.Card.added()
		if added.IsZero() || !added.After(j.since) || added.After(cutoff) {
			continue
		}
		if recent[e.Card.ContentID] {
			continue
		}
		if len(types) > 0 && !types[e.Card.Type] {
			continue
		}
		if len(libs) > 0 && !libs[e.Library.ID] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func setOf(list []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			m[v] = true
		}
	}
	return m
}

// libraries lists the server's libraries, or one catch-all entry when the
// host cannot list them.
func (a *App) libraries(ctx context.Context) []*pluginv1.Library {
	if a.libs != nil {
		return a.libs
	}
	host := sdkruntime.Host()
	if host != nil {
		libs, err := host.ListLibraries(ctx, "")
		if err == nil && len(libs) > 0 {
			return libs
		}
		if err != nil {
			a.log.Warn("list libraries", "error", err)
		}
	}
	return []*pluginv1.Library{{Id: "", Name: "", MediaType: ""}}
}

// collect reads items added after since across every library, newest first:
// movies by their catalog date, episodes from the recorded arrivals (see
// tvdetect.go for why the catalog date doesn't work for episodes).
func (a *App) collect(ctx context.Context, client *SiloClient, since time.Time, limit int) ([]Entry, error) {
	var (
		out  []Entry
		seen = map[string]bool{}
		errs []error
	)
	eps, err := a.episodeEntries(ctx, client, since, limit)
	if err != nil {
		errs = append(errs, fmt.Errorf("episodes: %w", err))
	}
	for _, e := range eps {
		seen[e.Card.ContentID] = true
		out = append(out, e)
	}
	for _, lib := range a.libraries(ctx) {
		for _, t := range libraryTypes(lib.GetMediaType()) {
			if t == "episode" {
				continue
			}
			cards, err := client.Recent(ctx, lib.GetId(), t, since, limit)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", nameOr(lib.GetName(), "catalog"), t, err))
			}
			for _, c := range cards {
				if c.ContentID == "" || seen[c.ContentID] {
					continue
				}
				seen[c.ContentID] = true
				out = append(out, Entry{Card: c, Library: LibraryRef{ID: lib.GetId(), Name: lib.GetName()}})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Card.added().After(out[j].Card.added()) })
	if len(errs) > 0 {
		return out, joinErrors(errs)
	}
	return out, nil
}

// enrich adds series details (show synopsis, poster) to episodes and
// details (tagline, cast) to movies.
func (a *App) enrich(ctx context.Context, client *SiloClient, entries []Entry) {
	for i := range entries {
		e := &entries[i]
		switch e.Card.Type {
		case "episode":
			if e.Card.SeriesID != "" {
				e.Series = a.detail(ctx, client, e.Card.SeriesID)
			}
			// Catalog cards carry the season poster; the episode's own still
			// and air date are on its detail.
			e.Episode = a.detail(ctx, client, e.Card.ContentID)
		case "movie":
			e.Movie = a.detail(ctx, client, e.Card.ContentID)
		}
	}
}

func (a *App) detail(ctx context.Context, client *SiloClient, id string) *Detail {
	a.detailMu.Lock()
	c, ok := a.details[id]
	a.detailMu.Unlock()
	if ok && time.Since(c.at) < detailTTL {
		return c.d
	}
	d, err := client.Item(ctx, id)
	if err != nil {
		a.log.Debug("item detail", "id", id, "error", err)
		return nil
	}
	a.detailMu.Lock()
	if len(a.details) > 2000 {
		a.details = map[string]cachedDetail{}
	}
	a.details[id] = cachedDetail{d: d, at: time.Now()}
	a.detailMu.Unlock()
	return d
}

// freshDetail reads an item's detail from Silo, bypassing the cache, so its
// presigned artwork URLs have their full lifetime ahead of them.
func (a *App) freshDetail(ctx context.Context, client *SiloClient, id string) *Detail {
	// One run refreshes the same show for every episode and every recipient.
	// A copy read moments ago still has nearly all of its URL lifetime left,
	// so reuse it instead of asking Silo again.
	a.detailMu.Lock()
	if c, ok := a.details[id]; ok && time.Since(c.at) < freshWindow {
		a.detailMu.Unlock()
		return c.d
	}
	delete(a.details, id)
	a.detailMu.Unlock()
	return a.detail(ctx, client, id)
}

// refreshArtwork re-reads the details of entries just before they are
// rendered and sent, replacing artwork URLs that may have expired while the
// entries waited in the cache, a scan batch or a rate limit hold. It also does
// what enrich does.
func (a *App) refreshArtwork(ctx context.Context, client *SiloClient, entries []Entry, compact bool, movieBudget int) {
	for i := range entries {
		e := &entries[i]
		if e.Card.ContentID == "" {
			continue
		}
		if compact && e.Card.Type == "movie" {
			// Each read is paced, so a summary of hundreds of movies only
			// refreshes as many as can carry an image.
			if movieBudget <= 0 {
				continue
			}
			movieBudget--
		}
		if compact && e.Card.Type == "episode" {
			// A summary shows the show's card only, so skip the per-episode read.
			if e.Card.SeriesID != "" {
				if s := a.freshDetail(ctx, client, e.Card.SeriesID); s != nil {
					e.Series = s
				}
			}
			continue
		}
		if d := a.freshDetail(ctx, client, e.Card.ContentID); d != nil {
			e.Card.PosterURL = d.PosterURL
			e.Card.BackdropURL = d.BackdropURL
			e.Card.PosterIsEpisodeStill = d.PosterIsEpisodeStill
			e.Card.BackdropIsEpisodeStill = d.BackdropIsEpisodeStill
			switch e.Card.Type {
			case "episode":
				e.Episode = d
			case "movie":
				e.Movie = d
			}
		}
		if e.Card.Type == "episode" && e.Card.SeriesID != "" {
			if s := a.freshDetail(ctx, client, e.Card.SeriesID); s != nil {
				e.Series = s
			}
		}
	}
}

// artworkFetcher downloads artwork for embedding and logs failures, which
// Render otherwise handles silently by leaving the image out.
func (a *App) artworkFetcher(client *SiloClient) func(context.Context, string) ([]byte, string, error) {
	return func(ctx context.Context, raw string) ([]byte, string, error) {
		data, ct, err := client.Image(ctx, raw)
		if err != nil {
			a.log.Warn("artwork download failed; leaving it out of the email", "url", redactURL(raw), "error", err)
		}
		return data, ct, err
	}
}

// redactURL drops the query string, which holds the presigned signature.
func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

func (a *App) env(s Settings, cfg Config, ep endpoints, r Recipient, mode string, now time.Time) RenderEnv {
	env := RenderEnv{
		Settings:     s,
		ServerName:   cfg.ServerName,
		PublicURL:    ep.public,
		InternalBase: ep.internal,
		Recipient:    r,
		Mode:         mode,
		Now:          now,
	}
	env.PluginBase = ep.pluginBase
	env.BackdropBG = func(seriesID string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.backdropBackgroundURL(ctx, env, seriesID)
	}
	if ep.pluginBase != "" && r.Token != "" {
		env.ManageURL = ep.pluginBase + "/public/manage?t=" + url.QueryEscape(r.Token)
		env.UnsubscribeURL = ep.pluginBase + "/public/unsubscribe?t=" + url.QueryEscape(r.Token)
	}
	return env
}

func (a *App) deliver(ctx context.Context, cfg Config, client *SiloClient, env RenderEnv, entries []Entry) (string, error) {
	a.refreshArtwork(ctx, client, entries, env.Compact, env.Settings.Delivery.MaxImages)
	rendered, err := Render(ctx, env, entries, a.artworkFetcher(client))
	if err != nil {
		return "", err
	}
	send := sendMail
	if a.send != nil {
		send = a.send
	}
	err = send(ctx, cfg.SMTP, Outgoing{
		To:             env.Recipient.Email,
		ToName:         env.Recipient.Name,
		Rendered:       rendered,
		UnsubscribeURL: env.UnsubscribeURL,
	})
	return rendered.Subject, err
}

// latest returns the newest items regardless of what anyone was sent, for
// previews and test emails.
func (a *App) latest(ctx context.Context, client *SiloClient, perType int) ([]Entry, error) {
	entries, err := a.collect(ctx, client, time.Time{}, perType)
	var movies, episodes []Entry
	for _, e := range entries {
		if e.Card.Type == "movie" && len(movies) < perType {
			movies = append(movies, e)
		}
		if e.Card.Type == "episode" && len(episodes) < perType {
			episodes = append(episodes, e)
		}
	}
	if len(episodes) == 0 {
		episodes = a.newestEpisodes(ctx, client, perType)
	}
	out := append(episodes, movies...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Card.added().After(out[j].Card.added()) })
	a.enrich(ctx, client, out)
	return out, err
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// joinedErrors reads as one line, like "a; b", and still lets errors.As find
// the errors inside it (the admin page needs to spot a rejected API key).
type joinedErrors []error

func (j joinedErrors) Error() string {
	parts := make([]string, len(j))
	for i, e := range j {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

func (j joinedErrors) Unwrap() []error { return j }

func joinErrors(errs []error) error { return joinedErrors(errs) }
