package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

//go:embed web/admin.html
var adminPage []byte

//go:embed web/me.html
var mePage []byte

type routeServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	app *App
}

type request struct {
	method  string
	path    string
	headers map[string]string
	body    []byte
	query   map[string]any
}

func (r request) header(name string) string {
	for k, v := range r.headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func (r request) q(name string) string {
	if v, ok := r.query[name]; ok {
		switch t := v.(type) {
		case string:
			return t
		case []any:
			if len(t) > 0 {
				return fmt.Sprint(t[0])
			}
		default:
			return fmt.Sprint(t)
		}
	}
	return ""
}

func (s *routeServer) Handle(ctx context.Context, in *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	s.app.ensureLoop()
	req := request{method: in.GetMethod(), path: in.GetPath(), headers: in.GetHeaders(), body: in.GetBody()}
	if in.GetQuery() != nil {
		req.query = in.GetQuery().AsMap()
	}
	path := req.path
	switch {
	case path == "/admin/" || path == "/admin":
		return page(adminPage), nil
	case path == "/me/" || path == "/me":
		return page(mePage), nil
	case strings.HasPrefix(path, "/api/admin/"):
		return s.app.adminAPI(ctx, req, strings.TrimPrefix(path, "/api/admin/")), nil
	case strings.HasPrefix(path, "/api/me/"):
		return s.app.meAPI(ctx, req, strings.TrimPrefix(path, "/api/me/")), nil
	case strings.HasPrefix(path, "/public/"):
		return s.app.publicRoute(ctx, req, strings.TrimPrefix(path, "/public/")), nil
	}
	return jsonError(404, "not found"), nil
}

func page(body []byte) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":  "text/html; charset=utf-8",
			"Cache-Control": "no-store",
		},
		Body: body,
	}
}

func jsonResp(status int, v any) *pluginv1.HandleHTTPResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return jsonError(500, err.Error())
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       body,
	}
}

func jsonError(status int, msg string) *pluginv1.HandleHTTPResponse {
	return jsonResp(status, map[string]string{"error": msg})
}

func decode(req request, v any) error {
	if len(req.body) == 0 {
		return errors.New("request body is empty")
	}
	return json.Unmarshal(req.body, v)
}

// ---- Admin API ------------------------------------------------------------

type recipientView struct {
	Recipient
	LastSent    *time.Time `json:"last_sent,omitempty"`
	NextDue     *time.Time `json:"next_due,omitempty"`
	Failures    int        `json:"failures,omitempty"`
	NextAttempt *time.Time `json:"next_attempt,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	HeldUntil   *time.Time `json:"held_until,omitempty"`
	Active      bool       `json:"active"`
}

type libraryView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
}

func (a *App) adminAPI(ctx context.Context, req request, sub string) *pluginv1.HandleHTTPResponse {
	switch {
	case sub == "state" && req.method == "GET":
		return a.adminState(ctx)

	case sub == "settings" && req.method == "PUT":
		var body struct {
			Settings Settings `json:"settings"`
		}
		if err := decode(req, &body); err != nil {
			return jsonError(400, err.Error())
		}
		if err := validateTemplates(body.Settings); err != nil {
			return jsonError(422, err.Error())
		}
		a.store.mu.Lock()
		err := a.store.SaveSettings(ctx, body.Settings)
		a.store.mu.Unlock()
		if err != nil {
			return jsonError(500, err.Error())
		}
		return a.adminState(ctx)

	case sub == "preview" && req.method == "POST":
		var body struct {
			Settings Settings `json:"settings"`
			Mode     string   `json:"mode"`
			Source   string   `json:"source"` // latest | sample
		}
		if err := decode(req, &body); err != nil {
			return jsonError(400, err.Error())
		}
		body.Settings.normalize()
		rendered, note, err := a.preview(ctx, body.Settings, body.Mode, body.Source)
		if err != nil {
			return jsonResp(200, map[string]any{"error": err.Error(), "note": note})
		}
		return jsonResp(200, map[string]any{"subject": rendered.Subject, "html": rendered.HTML, "text": rendered.Text, "note": note})

	case sub == "test" && req.method == "POST":
		var body struct {
			Settings *Settings `json:"settings"`
			Email    string    `json:"email"`
			Mode     string    `json:"mode"`
			// Pick narrows the test to the newest single item of a type:
			// "episode" or "movie". Empty sends the newest few of each.
			Pick string `json:"pick"`
			// Items sends a test with exactly these episodes or movies, by
			// content id, in this order.
			Items []string `json:"items"`
		}
		if err := decode(req, &body); err != nil {
			return jsonError(400, err.Error())
		}
		subject, count, err := a.sendTest(ctx, body.Settings, body.Email, body.Mode, body.Pick, body.Items)
		if err != nil {
			return jsonError(502, err.Error())
		}
		return jsonResp(200, map[string]any{"subject": subject, "items": count})

	case sub == "run" && req.method == "POST":
		// Sending can take minutes, longer than a web request lives, so the
		// run goes in the background and the status report shows the result.
		started := a.startRun("run now from admin page")
		return jsonResp(202, map[string]bool{"started": started})

	case sub == "recipients" && req.method == "POST":
		var body Recipient
		if err := decode(req, &body); err != nil {
			return jsonError(400, err.Error())
		}
		saved, err := a.saveAdminRecipient(ctx, body)
		if err != nil {
			return jsonError(422, err.Error())
		}
		return jsonResp(200, saved)

	case strings.HasPrefix(sub, "recipients/") && req.method == "DELETE":
		id := strings.TrimPrefix(sub, "recipients/")
		_, err := a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
			out := list[:0]
			for _, r := range list {
				if r.ID != id {
					out = append(out, r)
				}
			}
			return out, nil
		})
		if err != nil {
			return jsonError(500, err.Error())
		}
		a.dropState(ctx, id)
		return jsonResp(200, map[string]bool{"ok": true})

	case strings.HasPrefix(sub, "recipients/") && strings.HasSuffix(sub, "/rewind") && req.method == "POST":
		// Rewind lets the admin resend the last N hours to one recipient.
		id := strings.TrimSuffix(strings.TrimPrefix(sub, "recipients/"), "/rewind")
		var body struct {
			Hours int `json:"hours"`
		}
		if len(req.body) > 0 {
			if err := decode(req, &body); err != nil {
				return jsonError(400, "rewind: "+err.Error())
			}
		}
		if body.Hours <= 0 {
			body.Hours = 24
		}
		ids := []string{id}
		if list, err := a.store.Recipients(ctx); err == nil {
			for _, r := range list {
				if r.ID == id && r.Mode == "instant_daily" {
					ids = append(ids, deliveryStateID(r, "daily"))
				}
			}
		}
		for _, sid := range ids {
			st, _, err := a.store.State(ctx, sid)
			if err != nil {
				return jsonError(500, err.Error())
			}
			st.Watermark = time.Now().Add(-time.Duration(body.Hours) * time.Hour)
			st.LastRun = time.Time{}
			st.Recent = nil
			st.NextAttempt = time.Time{}
			st.Failures = 0
			st.LastInstant = time.Time{}
			st.DayCount = 0
			if err := a.store.SaveState(ctx, sid, st); err != nil {
				return jsonError(500, err.Error())
			}
		}
		return jsonResp(200, map[string]bool{"ok": true})
	}
	return jsonError(404, "unknown admin endpoint")
}

func (a *App) adminState(ctx context.Context) *pluginv1.HandleHTTPResponse {
	cfg := a.config()
	settings, err := a.store.Settings(ctx)
	if err != nil {
		return jsonError(500, err.Error())
	}
	recipients, err := a.store.Recipients(ctx)
	if err != nil {
		return jsonError(500, err.Error())
	}
	views := make([]recipientView, 0, len(recipients))
	for _, r := range recipients {
		v := recipientView{Recipient: r, Active: r.active(settings)}
		v.Token = ""
		v.VerifyToken = ""
		for _, mode := range deliveries(r.Mode) {
			st, found, err := a.store.State(ctx, deliveryStateID(r, mode))
			if err != nil || !found {
				continue
			}
			if !st.LastSent.IsZero() && (v.LastSent == nil || st.LastSent.After(*v.LastSent)) {
				t := st.LastSent
				v.LastSent = &t
			}
			v.Failures = max(v.Failures, st.Failures)
			if st.LastError != "" && v.LastError == "" {
				v.LastError = st.LastError
			}
			if !st.NextAttempt.IsZero() && (v.NextAttempt == nil || st.NextAttempt.Before(*v.NextAttempt)) {
				t := st.NextAttempt
				v.NextAttempt = &t
			}
			if mode == "instant" {
				if held := instantHeldUntil(st, settings, time.Now()); !held.IsZero() {
					v.HeldUntil = &held
				}
			}
		}
		if next := nextDue(r.Mode, settings, time.Now()); !next.IsZero() {
			v.NextDue = &next
		}
		views = append(views, v)
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Created.Before(views[j].Created) })
	logs, _ := a.store.Log(ctx)
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
	ep := a.endpoints(ctx)
	return jsonResp(200, map[string]any{
		"settings":        settings,
		"defaults":        defaultSettings(),
		"recipients":      views,
		"libraries":       a.libraryViews(ctx),
		"max_age_options": maxAgeOptions(settings),
		"problems":        cfg.problems(),
		"warnings":        a.health(settings, recipients, logs),
		"report":          a.report(),
		"log":             logs,
		"server": map[string]any{
			"name":        cfg.ServerName,
			"public_url":  ep.public,
			"plugin_base": ep.pluginBase,
			"smtp_host":   cfg.SMTP.Host,
			"from":        cfg.SMTP.FromAddress,
			"timezone":    settings.location().String(),
			"now":         time.Now().In(settings.location()).Format("Mon 2 Jan 15:04 MST"),
		},
	})
}

func nextDue(mode string, s Settings, now time.Time) time.Time {
	switch mode {
	case "daily", "instant_daily":
		return nextBoundary(now, s.location(), -1, s.Schedule.DailyHour)
	case "weekly":
		return nextBoundary(now, s.location(), s.Schedule.WeeklyDay, s.Schedule.WeeklyHour)
	}
	return time.Time{}
}

func (a *App) libraryViews(ctx context.Context) []libraryView {
	var out []libraryView
	for _, l := range a.libraries(ctx) {
		if l.GetId() == "" || libraryTypes(l.GetMediaType()) == nil {
			continue
		}
		out = append(out, libraryView{ID: l.GetId(), Name: l.GetName(), MediaType: l.GetMediaType()})
	}
	return out
}

func validateTemplates(s Settings) error {
	sample := buildData(RenderEnv{Settings: s, ServerName: "Silo", Mode: "preview", Now: time.Now()}, sampleEntries(), newImageSet(true, ""), true)
	_, err := renderTemplates(s, sample)
	return err
}

func (a *App) preview(ctx context.Context, s Settings, mode, source string) (*Rendered, string, error) {
	if mode == "" {
		mode = "instant"
	}
	cfg := a.config()
	ep := a.endpoints(ctx)
	var entries []Entry
	note := ""
	if source != "sample" && cfg.APIKey != "" {
		latest, err := a.latest(ctx, a.client(ctx, ep), 3)
		if err != nil {
			note = "Couldn't read everything from the catalog: " + err.Error()
		}
		entries = latest
	}
	if len(entries) == 0 {
		entries = sampleEntries()
		if note == "" {
			note = "Showing sample data."
			if cfg.APIKey == "" {
				note = "Showing sample data: add a Silo API key in the plugin settings to preview real items."
			}
		}
	} else if note == "" {
		note = fmt.Sprintf("Showing the %d newest items on the server.", len(entries))
	}
	env := a.env(s, cfg, ep, Recipient{Name: "Alex Example", Email: "alex@example.com", Token: "preview"}, mode, time.Now())
	env.Preview = true
	r, err := Render(ctx, env, entries, nil)
	return r, note, err
}

func (a *App) sendTest(ctx context.Context, override *Settings, email, mode, pick string, items []string) (string, int, error) {
	cfg := a.config()
	if cfg.SMTP.Host == "" || cfg.SMTP.FromAddress == "" {
		return "", 0, errors.New("fill in the plugin's Outgoing mail (SMTP) settings first")
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil {
		return "", 0, fmt.Errorf("%q isn't a valid email address", email)
	}
	settings, err := a.store.Settings(ctx)
	if err != nil {
		return "", 0, err
	}
	if override != nil {
		settings = *override
		settings.normalize()
	}
	if mode == "" {
		mode = "test"
	}
	ep := a.endpoints(ctx)
	client := a.client(ctx, ep)
	var entries []Entry
	if len(items) > 0 {
		if cfg.APIKey == "" {
			return "", 0, errors.New("add a Silo API key in the plugin settings first")
		}
		entries, err = a.chosenEntries(ctx, client, items)
		if err != nil {
			return "", 0, err
		}
	} else if cfg.APIKey != "" {
		entries, _ = a.latest(ctx, client, 3)
	}
	fetch := a.artworkFetcher(client)
	if len(entries) > 0 {
		a.refreshArtwork(ctx, client, entries, false, 0)
	} else {
		entries = sampleEntries()
		fetch = nil
	}
	if pick == "episode" || pick == "movie" {
		entries = newestOfType(entries, pick)
		if len(entries) == 0 {
			return "", 0, fmt.Errorf("there's no %s on the server to test with", pick)
		}
	}
	env := a.env(settings, cfg, ep, Recipient{Email: addr.Address, Name: addr.Name}, mode, time.Now())
	rendered, err := Render(ctx, env, entries, fetch)
	if err != nil {
		return "", 0, err
	}
	err = sendMail(ctx, cfg.SMTP, Outgoing{To: addr.Address, ToName: addr.Name, Rendered: rendered})
	entry := LogEntry{Time: time.Now(), Recipient: addr.Address, Mode: "test", Items: len(entries), Subject: rendered.Subject}
	if err != nil {
		entry.Error = err.Error()
	}
	pctx, done := persistCtx(ctx)
	if logErr := a.store.AppendLog(pctx, entry); logErr != nil {
		a.log.Warn("append log", "error", logErr)
	}
	done()
	return rendered.Subject, len(entries), err
}

// chosenEntries builds entries for specific episodes or movies, keeping the
// given order.
func (a *App) chosenEntries(ctx context.Context, client *SiloClient, ids []string) ([]Entry, error) {
	if len(ids) > 100 {
		ids = ids[:100]
	}
	now := time.Now()
	var out []Entry
	for i, id := range ids {
		d := a.detail(ctx, client, strings.TrimSpace(id))
		if d == nil {
			return nil, fmt.Errorf("couldn't find %q on the server", id)
		}
		at := now.Add(-time.Duration(i) * time.Second)
		switch d.Type {
		case "episode":
			e, _ := a.episodeEntry(ctx, client, d.ContentID, at, LibraryRef{})
			out = append(out, e)
		case "movie":
			card := d.Card
			card.AddedAt = at.UTC().Format(time.RFC3339Nano)
			out = append(out, Entry{Card: card, Movie: d})
		default:
			return nil, fmt.Errorf("%q is a %s; pick episodes or movies", id, d.Type)
		}
	}
	a.enrich(ctx, client, out)
	return out, nil
}

// newestOfType keeps the newest single entry of one type.
func newestOfType(entries []Entry, t string) []Entry {
	for _, e := range entries {
		if e.Card.Type == t {
			return []Entry{e}
		}
	}
	return nil
}

func cleanMode(m string) string {
	switch m {
	case "daily", "weekly", "instant_daily":
		return m
	}
	return "instant"
}

// maxAgeChoices are the "new means added within" options offered to
// recipients, in hours; 0 means the server default.
var maxAgeChoices = []int{24, 48, 72, 168, 336, 720}

const maxAgeLimit = 720

func cleanMaxAge(h int) int {
	for _, c := range maxAgeChoices {
		if h == c {
			return h
		}
	}
	return 0
}

// maxAgeLabel describes a max age in hours, such as "48 hours" or "1 week".
func maxAgeLabel(h int) string {
	switch {
	case h%168 == 0:
		return plural(h/168, "week", "weeks")
	case h%24 == 0 && h > 48:
		return plural(h/24, "day", "days")
	}
	return plural(h, "hour", "hours")
}

type maxAgeOption struct {
	Hours int    `json:"hours"`
	Label string `json:"label"`
}

// maxAgeOptions lists the choices for pages, with the server default first.
func maxAgeOptions(s Settings) []maxAgeOption {
	out := []maxAgeOption{{0, "Server default (" + maxAgeLabel(s.Delivery.MaxAgeHours) + ")"}}
	for _, h := range maxAgeChoices {
		out = append(out, maxAgeOption{h, maxAgeLabel(h)})
	}
	return out
}

func cleanTypes(types []string) []string {
	var out []string
	for _, t := range types {
		if t == "movie" || t == "episode" {
			out = append(out, t)
		}
	}
	return out
}

func (a *App) saveAdminRecipient(ctx context.Context, in Recipient) (Recipient, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(in.Email))
	if err != nil {
		return Recipient{}, fmt.Errorf("%q isn't a valid email address", in.Email)
	}
	var saved Recipient
	_, err = a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
		for i, r := range list {
			if r.ID == in.ID && in.ID != "" {
				r.Name = strings.TrimSpace(in.Name)
				r.Email = addr.Address
				r.Enabled = in.Enabled
				r.Mode = cleanMode(in.Mode)
				r.Types = cleanTypes(in.Types)
				r.Libraries = in.Libraries
				r.MaxAgeHours = cleanMaxAge(in.MaxAgeHours)
				if r.Source == "admin" {
					r.Verified = true
				}
				list[i] = r
				saved = r
				return list, nil
			}
		}
		for _, r := range list {
			if strings.EqualFold(r.Email, addr.Address) {
				return nil, fmt.Errorf("%s is already a recipient", addr.Address)
			}
		}
		saved = Recipient{
			ID:          newID(),
			Name:        strings.TrimSpace(in.Name),
			Email:       addr.Address,
			Enabled:     in.Enabled,
			Mode:        cleanMode(in.Mode),
			Types:       cleanTypes(in.Types),
			Libraries:   in.Libraries,
			MaxAgeHours: cleanMaxAge(in.MaxAgeHours),
			Source:      "admin",
			Verified:    true,
			Token:       newToken(),
			Created:     time.Now(),
		}
		return append(list, saved), nil
	})
	if err == nil {
		a.dropDigestState(ctx, saved)
	}
	saved.Token = ""
	return saved, err
}

// dropDigestState forgets the digest half of the combined mode once a
// recipient stops using it, so switching back later starts the digest from
// now instead of from a stale watermark.
func (a *App) dropDigestState(ctx context.Context, r Recipient) {
	if r.ID != "" && r.Mode != "instant_daily" {
		a.deleteState(ctx, dailyStateID(r.ID))
	}
}

// deleteState forgets one delivery's state, logging a failure so a stale
// watermark can't linger unnoticed.
func (a *App) deleteState(ctx context.Context, id string) {
	if err := a.store.DeleteState(ctx, id); err != nil {
		a.log.Warn("delete state", "id", id, "error", err)
	}
}

// dropState forgets both deliveries of a recipient that was removed.
func (a *App) dropState(ctx context.Context, id string) {
	a.deleteState(ctx, id)
	a.deleteState(ctx, dailyStateID(id))
}

// ---- Self-service API -----------------------------------------------------

func (a *App) meAPI(ctx context.Context, req request, sub string) *pluginv1.HandleHTTPResponse {
	userID := req.header("X-Silo-User-Id")
	if userID == "" {
		return jsonError(401, "sign in to manage your email alerts")
	}
	settings, err := a.store.Settings(ctx)
	if err != nil {
		return jsonError(500, err.Error())
	}
	if !settings.SelfService.Enabled {
		return jsonResp(200, map[string]any{"enabled": false})
	}
	userName := req.header("X-Silo-User-Name")
	if p := req.header("X-Silo-Profile-Name"); userName == "" && p != "" {
		userName = p
	}

	find := func(list []Recipient) int {
		for i, r := range list {
			if r.Source == "user" && r.UserID == userID {
				return i
			}
		}
		return -1
	}

	switch {
	case sub == "subscription" && req.method == "GET":
		list, err := a.store.Recipients(ctx)
		if err != nil {
			return jsonError(500, err.Error())
		}
		return jsonResp(200, a.meView(ctx, settings, list, find(list), userName))

	case sub == "subscription" && req.method == "PUT":
		var body struct {
			Name        string   `json:"name"`
			Email       string   `json:"email"`
			Mode        string   `json:"mode"`
			Types       []string `json:"types"`
			Libraries   []string `json:"libraries"`
			MaxAgeHours int      `json:"max_age_hours"`
			Enabled     bool     `json:"enabled"`
		}
		if err := decode(req, &body); err != nil {
			return jsonError(400, err.Error())
		}
		addr, err := mail.ParseAddress(strings.TrimSpace(body.Email))
		if err != nil {
			return jsonError(422, fmt.Sprintf("%q isn't a valid email address", body.Email))
		}
		var needVerify *Recipient
		list, err := a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
			for _, r := range list {
				if strings.EqualFold(r.Email, addr.Address) && !(r.Source == "user" && r.UserID == userID) {
					return nil, errors.New("that address already gets alerts; ask an admin if you want it moved to your account")
				}
			}
			i := find(list)
			if i < 0 {
				list = append(list, Recipient{ID: newID(), Source: "user", UserID: userID, Token: newToken(), Created: time.Now()})
				i = len(list) - 1
			}
			r := list[i]
			r.Name = strings.TrimSpace(body.Name)
			if r.Name == "" {
				r.Name = userName
			}
			r.Mode = cleanMode(body.Mode)
			r.Types = cleanTypes(body.Types)
			r.Libraries = body.Libraries
			r.MaxAgeHours = cleanMaxAge(body.MaxAgeHours)
			r.Enabled = body.Enabled
			if !strings.EqualFold(r.Email, addr.Address) || (!r.Verified && r.PendingEmail == "") {
				if settings.SelfService.RequireVerification {
					r.PendingEmail = addr.Address
					r.VerifyToken = newToken()
					if r.Email == "" {
						r.Email = addr.Address
						r.Verified = false
					}
					pending := r
					needVerify = &pending
				} else {
					r.Email = addr.Address
					r.Verified = true
					r.PendingEmail = ""
					r.VerifyToken = ""
				}
			}
			list[i] = r
			return list, nil
		})
		if err != nil {
			return jsonError(422, err.Error())
		}
		if i := find(list); i >= 0 {
			a.dropDigestState(ctx, list[i])
		}
		msg := "Saved."
		if needVerify != nil {
			if err := a.sendVerification(ctx, settings, *needVerify); err != nil {
				msg = "Saved, but the confirmation email couldn't be sent: " + err.Error()
			} else {
				msg = "Check " + needVerify.PendingEmail + " for a confirmation link."
			}
		}
		view := a.meView(ctx, settings, list, find(list), userName)
		view["message"] = msg
		return jsonResp(200, view)

	case sub == "subscription/resend" && req.method == "POST":
		list, err := a.store.Recipients(ctx)
		if err != nil {
			return jsonError(500, err.Error())
		}
		i := find(list)
		if i < 0 || list[i].VerifyToken == "" {
			return jsonError(400, "there's nothing waiting for confirmation")
		}
		if err := a.sendVerification(ctx, settings, list[i]); err != nil {
			return jsonError(502, err.Error())
		}
		return jsonResp(200, map[string]string{"message": "Sent another confirmation email."})

	case sub == "subscription" && req.method == "DELETE":
		var removed string
		_, err := a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
			out := list[:0]
			for _, r := range list {
				if r.Source == "user" && r.UserID == userID {
					removed = r.ID
					continue
				}
				out = append(out, r)
			}
			return out, nil
		})
		if err != nil {
			return jsonError(500, err.Error())
		}
		if removed != "" {
			a.dropState(ctx, removed)
		}
		return jsonResp(200, a.meView(ctx, settings, nil, -1, userName))
	}
	return jsonError(404, "unknown endpoint")
}

func (a *App) meView(ctx context.Context, s Settings, list []Recipient, i int, userName string) map[string]any {
	view := map[string]any{
		"enabled":              true,
		"require_verification": s.SelfService.RequireVerification,
		"libraries":            a.libraryViews(ctx),
		"user_name":            userName,
		"max_age_options":      maxAgeOptions(s),
		"schedule": map[string]any{
			"daily_hour":  s.Schedule.DailyHour,
			"weekly_day":  s.Schedule.WeeklyDay,
			"weekly_hour": s.Schedule.WeeklyHour,
			"timezone":    s.location().String(),
		},
		"server_name": a.config().ServerName,
	}
	if i >= 0 {
		r := list[i]
		view["subscription"] = map[string]any{
			"name":          r.Name,
			"email":         r.Email,
			"pending_email": r.PendingEmail,
			"verified":      r.Verified,
			"enabled":       r.Enabled,
			"mode":          r.Mode,
			"types":         r.Types,
			"libraries":     r.Libraries,
			"max_age_hours": r.MaxAgeHours,
		}
	}
	return view
}

func (a *App) sendVerification(ctx context.Context, s Settings, r Recipient) error {
	cfg := a.config()
	ep := a.endpoints(ctx)
	if ep.pluginBase == "" {
		return errors.New("Silo has no public URL (SILO_PUBLIC_URL), so a confirmation link can't be built")
	}
	data := map[string]any{
		"server_name":          cfg.ServerName,
		"recipient_first_name": firstName(r.Name),
		"verify_url":           ep.pluginBase + "/public/verify?t=" + url.QueryEscape(r.VerifyToken),
		"accent_color":         s.Theme.AccentColor,
		"background_color":     s.Theme.BackgroundColor,
		"card_color":           s.Theme.CardColor,
		"text_color":           s.Theme.TextColor,
		"muted_color":          s.Theme.MutedColor,
	}
	htmlBody, err := renderOne("verification", verifyHTML, false, data)
	if err != nil {
		return err
	}
	textBody, err := renderOne("verification", verifyText, true, data)
	if err != nil {
		return err
	}
	return sendMail(ctx, cfg.SMTP, Outgoing{
		To:       r.PendingEmail,
		ToName:   r.Name,
		Rendered: &Rendered{Subject: "Confirm your email for " + cfg.ServerName + " alerts", HTML: htmlBody, Text: textBody},
	})
}

// ---- Public pages ---------------------------------------------------------

func (a *App) publicRoute(ctx context.Context, req request, sub string) *pluginv1.HandleHTTPResponse {
	if sub == "bg" && req.method == "GET" {
		return a.serveBackdrop(ctx, req)
	}
	if sub == "open" && req.method == "GET" {
		return a.serveOpen(ctx, req)
	}
	token := req.q("t")
	form := url.Values{}
	if req.method == "POST" {
		if v, err := url.ParseQuery(string(req.body)); err == nil {
			form = v
		}
		if token == "" {
			token = form.Get("t")
		}
	}
	if token == "" {
		return simplePage(400, "Link not valid", "This link is missing its token.", "")
	}
	settings, _ := a.store.Settings(ctx)
	switch sub {
	case "verify":
		var who string
		_, err := a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
			for i, r := range list {
				if r.PendingEmail != "" && tokenEq(r.VerifyToken, token) {
					r.Email = r.PendingEmail
					r.PendingEmail = ""
					r.VerifyToken = ""
					r.Verified = true
					list[i] = r
					who = r.Email
					return list, nil
				}
			}
			return nil, errNotFound
		})
		if err != nil {
			return simplePage(404, "Link expired", "This confirmation link has already been used or replaced.", "")
		}
		return simplePage(200, "Address confirmed", who+" will now get new media alerts.", "")

	case "unsubscribe":
		r, ok := a.byToken(ctx, token)
		if !ok {
			return simplePage(404, "Link not valid", "This unsubscribe link isn't recognised. You may already be unsubscribed.", "")
		}
		if req.method == "POST" {
			if err := a.setEnabled(ctx, token, false); err != nil {
				return simplePage(500, "Something went wrong", err.Error(), "")
			}
			return simplePage(200, "Unsubscribed", r.Email+" won't get any more alerts. You can turn them back on from the Email alerts page in Silo.", "")
		}
		return simplePage(200, "Unsubscribe?", "Stop sending new media alerts to "+r.Email+"?",
			`<form method="post"><input type="hidden" name="t" value="`+html.EscapeString(token)+`"><button type="submit">Unsubscribe</button></form>`)

	case "manage":
		r, ok := a.byToken(ctx, token)
		if !ok {
			return simplePage(404, "Link not valid", "This link isn't recognised.", "")
		}
		msg := ""
		if req.method == "POST" {
			_, err := a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
				for i, x := range list {
					if tokenEq(x.Token, token) {
						x.Mode = cleanMode(form.Get("mode"))
						if v := form.Get("max_age"); v != "" {
							n, _ := strconv.Atoi(v)
							x.MaxAgeHours = cleanMaxAge(n)
						}
						// Unticking both would read as "everything", so keep the
						// previous choice instead.
						if types := cleanTypes(form["types"]); len(types) == 2 {
							x.Types = nil
						} else if len(types) == 1 {
							x.Types = types
						}
						x.Enabled = form.Get("enabled") == "on"
						list[i] = x
						r = x
						return list, nil
					}
				}
				return nil, errNotFound
			})
			if err != nil {
				return simplePage(500, "Something went wrong", err.Error(), "")
			}
			a.dropDigestState(ctx, r)
			msg = `<p class="ok">Saved.</p>`
		}
		return simplePage(200, "Your email alerts", "Settings for "+r.Email+".", msg+manageForm(token, r, settings))
	}
	return simplePage(404, "Not found", "", "")
}

var errNotFound = errors.New("not found")

func (a *App) byToken(ctx context.Context, token string) (Recipient, bool) {
	list, err := a.store.Recipients(ctx)
	if err != nil {
		return Recipient{}, false
	}
	for _, r := range list {
		if tokenEq(r.Token, token) {
			return r, true
		}
	}
	return Recipient{}, false
}

func (a *App) setEnabled(ctx context.Context, token string, enabled bool) error {
	_, err := a.store.UpdateRecipients(ctx, func(list []Recipient) ([]Recipient, error) {
		for i, r := range list {
			if tokenEq(r.Token, token) {
				r.Enabled = enabled
				list[i] = r
				return list, nil
			}
		}
		return nil, errNotFound
	})
	return err
}

func manageForm(token string, r Recipient, s Settings) string {
	checked := func(b bool) string {
		if b {
			return " checked"
		}
		return ""
	}
	types := setOf(r.Types)
	all := len(types) == 0
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	var b strings.Builder
	b.WriteString(`<form method="post"><input type="hidden" name="t" value="` + html.EscapeString(token) + `">`)
	b.WriteString(`<label class="row"><input type="checkbox" name="enabled"` + checked(r.Enabled) + `> Send me alerts</label>`)
	b.WriteString(`<fieldset><legend>How often</legend>`)
	b.WriteString(`<label class="row"><input type="radio" name="mode" value="instant"` + checked(r.Mode == "instant" || r.Mode == "") + `> As soon as something is added</label>`)
	b.WriteString(fmt.Sprintf(`<label class="row"><input type="radio" name="mode" value="instant_daily"%s> As soon as something is added, plus a daily digest at %02d:00</label>`, checked(r.Mode == "instant_daily"), s.Schedule.DailyHour))
	b.WriteString(fmt.Sprintf(`<label class="row"><input type="radio" name="mode" value="daily"%s> Daily digest at %02d:00</label>`, checked(r.Mode == "daily"), s.Schedule.DailyHour))
	b.WriteString(fmt.Sprintf(`<label class="row"><input type="radio" name="mode" value="weekly"%s> Weekly digest on %s at %02d:00</label>`, checked(r.Mode == "weekly"), days[s.Schedule.WeeklyDay], s.Schedule.WeeklyHour))
	b.WriteString(`</fieldset><fieldset><legend>Count as new if added within</legend><select name="max_age">`)
	for _, o := range maxAgeOptions(s) {
		sel := ""
		if o.Hours == r.MaxAgeHours {
			sel = " selected"
		}
		b.WriteString(fmt.Sprintf(`<option value="%d"%s>%s</option>`, o.Hours, sel, html.EscapeString(o.Label)))
	}
	b.WriteString(`</select></fieldset><fieldset><legend>What to include</legend>`)
	b.WriteString(`<label class="row"><input type="checkbox" name="types" value="episode"` + checked(all || types["episode"]) + `> TV episodes</label>`)
	b.WriteString(`<label class="row"><input type="checkbox" name="types" value="movie"` + checked(all || types["movie"]) + `> Movies</label>`)
	b.WriteString(`</fieldset><button type="submit">Save</button></form>`)
	return b.String()
}

func simplePage(status int, title, message, extra string) *pluginv1.HandleHTTPResponse {
	body := `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(title) + `</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--card:#fff;--text:#16181d;--muted:#5d6470;--accent:#6b4dff;--line:#e3e6ea}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#181b22;--text:#f2f3f5;--muted:#9aa0ab;--line:#2a2f39}}
body{margin:0;background:var(--bg);color:var(--text);font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;padding:40px 16px}
main{max-width:520px;margin:0 auto;background:var(--card);border:1px solid var(--line);border-radius:14px;padding:28px}
h1{font-size:22px;margin:0 0 8px}p{color:var(--muted);margin:0 0 18px}
button{background:var(--accent);color:#fff;border:0;border-radius:8px;padding:10px 18px;font:inherit;font-weight:600;cursor:pointer}
fieldset{border:1px solid var(--line);border-radius:10px;margin:0 0 16px;padding:10px 14px}legend{color:var(--muted);font-size:13px;padding:0 6px}
select{width:100%;background:var(--bg);color:var(--text);border:1px solid var(--line);border-radius:8px;padding:8px 10px;font:inherit;margin:5px 0}
.row{display:flex;gap:10px;align-items:center;padding:5px 0}.ok{color:#2e9d5b;font-weight:600}
</style></head><body><main><h1>` + html.EscapeString(title) + `</h1>`
	if message != "" {
		body += `<p>` + html.EscapeString(message) + `</p>`
	}
	body += extra + `</main></body></html>`
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers: map[string]string{
			"Content-Type":    "text/html; charset=utf-8",
			"Cache-Control":   "no-store",
			"Referrer-Policy": "no-referrer",
		},
		Body: []byte(body),
	}
}

// tokenEq compares a stored token with one from a link without leaking how
// much of it matched. An empty stored token never matches.
func tokenEq(stored, given string) bool {
	return stored != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(given)) == 1
}

// Warning is something the admin should know about even though the plugin is
// configured: sending is failing, paused or has nobody to send to.
type Warning struct {
	Level string `json:"level"` // bad | warn
	Text  string `json:"text"`
}

// health turns the last run and the send log into plain-language warnings.
func (a *App) health(settings Settings, recipients []Recipient, logs []LogEntry) []Warning {
	var out []Warning
	if r := a.report(); r != nil && r.KeyRejected {
		out = append(out, Warning{"bad", "Silo rejected the plugin's API key, so it can't see new media. Create a new admin key under Settings → API keys and paste it into this plugin's Silo connection."})
	}
	if settings.Delivery.Paused {
		out = append(out, Warning{"warn", "Sending is paused. Untick Paused under Delivery to send emails again."})
	}
	active := 0
	for _, r := range recipients {
		if r.active(settings) {
			active++
		}
	}
	if active == 0 {
		out = append(out, Warning{"warn", "Nobody is signed up. Add a recipient below or let users subscribe from their own Email alerts page."})
	}
	// logs are newest first; one recent failure is worth showing, an old one isn't.
	for _, l := range logs {
		if l.Mode == "test" {
			continue
		}
		if l.Error != "" && time.Since(l.Time) < 24*time.Hour {
			out = append(out, Warning{"bad", "The last email to " + l.Recipient + " failed: " + l.Error + ". Check the outgoing mail settings."})
		}
		break
	}
	if settings.Delivery.QuietEnabled {
		if until := quietUntil(time.Now(), settings); !until.IsZero() {
			out = append(out, Warning{"warn", "Quiet hours are on. Instant emails are held until " + until.In(settings.location()).Format("15:04") + "."})
		}
	}
	return out
}
