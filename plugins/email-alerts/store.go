package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// Settings is everything the admin page edits. It is stored as one JSON
// document in the installation's encrypted instance state.
type Settings struct {
	Templates   Templates   `json:"templates"`
	Theme       Theme       `json:"theme"`
	CustomVars  []CustomVar `json:"custom_vars"`
	Schedule    Schedule    `json:"schedule"`
	Delivery    Delivery    `json:"delivery"`
	SelfService SelfService `json:"self_service"`
}

type Templates struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
}

type Theme struct {
	AccentColor     string `json:"accent_color"`
	BackgroundColor string `json:"background_color"`
	CardColor       string `json:"card_color"`
	TextColor       string `json:"text_color"`
	MutedColor      string `json:"muted_color"`
	LogoURL         string `json:"logo_url"`
	HeaderTitle     string `json:"header_title"`
	Intro           string `json:"intro"`
	FooterText      string `json:"footer_text"`
	ButtonText      string `json:"button_text"`
	// BackdropBehindShow puts the show's backdrop behind its name and
	// synopsis, darkened by BackdropDim percent with the card colour.
	BackdropBehindShow bool `json:"backdrop_behind_show"`
	BackdropDim        int  `json:"backdrop_dim"`
	// OpenInApp routes item links through a page that opens the Silo app
	// when it's installed on the reader's phone.
	OpenInApp bool `json:"open_in_app"`
}

type CustomVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Schedule struct {
	Timezone   string `json:"timezone"`
	DailyHour  int    `json:"daily_hour"`
	WeeklyDay  int    `json:"weekly_day"` // 0 = Sunday
	WeeklyHour int    `json:"weekly_hour"`
}

type Delivery struct {
	Paused         bool `json:"paused"`
	SettleMinutes  int  `json:"settle_minutes"`
	MaxItems       int  `json:"max_items"`
	EmbedImages    bool `json:"embed_images"`
	MaxImages      int  `json:"max_images"`
	SynopsisLength int  `json:"synopsis_length"`
	// InstantGapMinutes is the least time between two instant emails to one
	// recipient; items that arrive sooner are held and sent together.
	InstantGapMinutes int `json:"instant_gap_minutes"`
	// InstantDailyCap limits instant emails per recipient per day (0 = no
	// limit); items past the cap go out in the first email of the next day.
	InstantDailyCap int `json:"instant_daily_cap"`
	// Quiet hours hold instant emails overnight. Anything that arrives
	// between QuietStart and QuietEnd (local hours, the window may wrap past
	// midnight) goes out together when the window ends.
	QuietEnabled bool `json:"quiet_enabled"`
	QuietStart   int  `json:"quiet_start"`
	QuietEnd     int  `json:"quiet_end"`
	// MaxAgeHours is the default for how recently an item must have been
	// added to be emailed at all. Anything older is never treated as new,
	// whatever a recipient's state says. Recipients can choose their own.
	MaxAgeHours int `json:"max_age_hours"`
	// BurstThreshold: a send with more items than this goes out as one
	// compact summary (a card per show, no episode lists, no cap) instead of
	// a long email. 0 turns it off.
	BurstThreshold int `json:"burst_threshold"`
}

type SelfService struct {
	Enabled             bool `json:"enabled"`
	RequireVerification bool `json:"require_verification"`
}

// Recipient is one address that receives alerts. Admins add recipients on
// the admin page; users add themselves on the self-service page.
type Recipient struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Email     string   `json:"email"`
	Enabled   bool     `json:"enabled"`
	Mode      string   `json:"mode"`      // instant | instant_daily | daily | weekly
	Types     []string `json:"types"`     // movie, episode; empty = all
	Libraries []string `json:"libraries"` // library ids; empty = all
	// MaxAgeHours is how recently an item must have been added to count as
	// new for this recipient; 0 uses the server default.
	MaxAgeHours int       `json:"max_age_hours,omitempty"`
	Source      string    `json:"source"` // admin | user
	UserID      string    `json:"user_id,omitempty"`
	Verified    bool      `json:"verified"`
	Token       string    `json:"token"`
	Created     time.Time `json:"created"`

	// PendingEmail and VerifyToken hold an address change a user has not
	// confirmed yet.
	PendingEmail string `json:"pending_email,omitempty"`
	VerifyToken  string `json:"verify_token,omitempty"`
}

// RecipientState tracks what a recipient has been sent.
type RecipientState struct {
	// Watermark is the added_at boundary up to which items were considered.
	Watermark time.Time `json:"watermark"`
	// LastRun is the last time the recipient was evaluated (digests use it).
	LastRun time.Time `json:"last_run"`
	// LastSent is the last time an email actually went out.
	LastSent time.Time `json:"last_sent"`
	// Recent is the content ids sent recently, newest last; it stops an item
	// whose added_at falls inside the overlap window from being sent twice.
	Recent []string `json:"recent"`
	// Failures and NextAttempt back off after a failed send.
	Failures    int       `json:"failures,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitempty"`
	// LastInstant, Day and DayCount drive the instant-email rate limit.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	// Claim marks a send in progress. Before emailing, a process writes a
	// claim and reads it back; if another process's claim is there instead,
	// it skips, so two plugin processes can't send the same items twice.
	Claim       *Claim    `json:"claim,omitempty"`
	LastInstant time.Time `json:"last_instant,omitempty"`
	Day         string    `json:"day,omitempty"`
	DayCount    int       `json:"day_count,omitempty"`
}

// Claim is one process's hold on a recipient's pending send.
type Claim struct {
	Nonce string    `json:"nonce"`
	PID   int       `json:"pid"`
	At    time.Time `json:"at"`
}

type LogEntry struct {
	Time      time.Time `json:"time"`
	Recipient string    `json:"recipient"`
	Mode      string    `json:"mode"`
	Items     int       `json:"items"`
	Subject   string    `json:"subject"`
	Error     string    `json:"error,omitempty"`
	// Started is when the run that sent this began, and PID the plugin
	// process that ran it. Two entries for the same item from different
	// processes mean runs overlapped across processes.
	Started time.Time `json:"started,omitzero"`
	PID     int       `json:"pid,omitempty"`
	// ItemIDs are the content ids that were emailed, for tracing a repeat.
	ItemIDs []string `json:"item_ids,omitempty"`
	// Burst marks an email sent as a compact summary of a large batch.
	Burst bool `json:"burst,omitempty"`
}

const (
	keySettings   = "settings"
	keyRecipients = "recipients"
	keyLog        = "log"
	keyEvents     = "handled-events"
	recentCap     = 600
	logCap        = 150
	eventsCap     = 300
)

func stateKey(id string) string { return "state:" + id }

var errNoHost = errors.New("silo host connection not ready yet")

// Store reads and writes instance state. Every process of an installation
// shares the same rows, so reads always go to the host; the mutex only
// serialises this process's read-modify-write cycles.
type Store struct {
	mu sync.Mutex
	// backend replaces the host's instance state in tests.
	backend stateBackend
}

type stateBackend interface {
	ReadInstanceState(ctx context.Context, key string) ([]byte, bool, error)
	WriteInstanceState(ctx context.Context, key string, value []byte) error
}

func (s *Store) host() stateBackend {
	if s.backend != nil {
		return s.backend
	}
	if h := sdkruntime.Host(); h != nil {
		return h
	}
	return nil
}

func (s *Store) read(ctx context.Context, key string, out any) (bool, error) {
	host := s.host()
	if host == nil {
		return false, errNoHost
	}
	raw, found, err := host.ReadInstanceState(ctx, key)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", key, err)
	}
	if !found || len(raw) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}

func (s *Store) write(ctx context.Context, key string, value any) error {
	host := s.host()
	if host == nil {
		return errNoHost
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := host.WriteInstanceState(ctx, key, raw); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	settings := defaultSettings()
	if _, err := s.read(ctx, keySettings, &settings); err != nil {
		return defaultSettings(), err
	}
	settings.normalize()
	return settings, nil
}

func (s *Store) SaveSettings(ctx context.Context, settings Settings) error {
	settings.normalize()
	return s.write(ctx, keySettings, settings)
}

func (s *Store) Recipients(ctx context.Context) ([]Recipient, error) {
	var list []Recipient
	_, err := s.read(ctx, keyRecipients, &list)
	return list, err
}

func (s *Store) SaveRecipients(ctx context.Context, list []Recipient) error {
	if list == nil {
		list = []Recipient{}
	}
	return s.write(ctx, keyRecipients, list)
}

// UpdateRecipients applies fn to the recipient list under the store lock.
func (s *Store) UpdateRecipients(ctx context.Context, fn func([]Recipient) ([]Recipient, error)) ([]Recipient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.Recipients(ctx)
	if err != nil {
		return nil, err
	}
	list, err = fn(list)
	if err != nil {
		return nil, err
	}
	return list, s.SaveRecipients(ctx, list)
}

func (s *Store) State(ctx context.Context, id string) (RecipientState, bool, error) {
	var st RecipientState
	found, err := s.read(ctx, stateKey(id), &st)
	return st, found, err
}

func (s *Store) SaveState(ctx context.Context, id string, st RecipientState) error {
	if len(st.Recent) > recentCap {
		st.Recent = st.Recent[len(st.Recent)-recentCap:]
	}
	return s.write(ctx, stateKey(id), st)
}

func (s *Store) DeleteState(ctx context.Context, id string) error {
	return s.write(ctx, stateKey(id), RecipientState{})
}

func (s *Store) Log(ctx context.Context) ([]LogEntry, error) {
	var entries []LogEntry
	_, err := s.read(ctx, keyLog, &entries)
	return entries, err
}

func (s *Store) AppendLog(ctx context.Context, add ...LogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.Log(ctx)
	if err != nil {
		return err
	}
	entries = append(entries, add...)
	if len(entries) > logCap {
		entries = entries[len(entries)-logCap:]
	}
	return s.write(ctx, keyLog, entries)
}

// MarkEvent records an event id and reports whether it was new. Silo
// delivers events at least once, so a retry must not trigger a second run.
func (s *Store) MarkEvent(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	if _, err := s.read(ctx, keyEvents, &ids); err != nil {
		return true, err
	}
	for _, seen := range ids {
		if seen == id {
			return false, nil
		}
	}
	ids = append(ids, id)
	if len(ids) > eventsCap {
		ids = ids[len(ids)-eventsCap:]
	}
	return true, s.write(ctx, keyEvents, ids)
}

func defaultSettings() Settings {
	return Settings{
		Templates: Templates{
			Subject: defaultSubjectTemplate,
			HTML:    defaultHTMLTemplate,
			Text:    defaultTextTemplate,
		},
		Theme: Theme{
			AccentColor:        "#7c5cff",
			BackgroundColor:    "#0f1115",
			CardColor:          "#181b22",
			TextColor:          "#f2f3f5",
			MutedColor:         "#9aa0ab",
			HeaderTitle:        "New on {{server_name}}",
			Intro:              "Here's what just landed on the server.",
			FooterText:         "You're receiving this because you signed up for new media alerts.",
			ButtonText:         "Watch now",
			BackdropBehindShow: true,
			BackdropDim:        72,
			OpenInApp:          true,
		},
		Schedule: Schedule{DailyHour: 18, WeeklyDay: 5, WeeklyHour: 18},
		Delivery: Delivery{
			SettleMinutes:     5,
			MaxItems:          60,
			EmbedImages:       true,
			MaxImages:         40,
			SynopsisLength:    320,
			InstantGapMinutes: 10,
			MaxAgeHours:       168,
			BurstThreshold:    25,
		},
		SelfService: SelfService{Enabled: true, RequireVerification: true},
	}
}

func (s *Settings) normalize() {
	def := defaultSettings()
	if strings.TrimSpace(s.Templates.Subject) == "" {
		s.Templates.Subject = def.Templates.Subject
	}
	if strings.TrimSpace(s.Templates.HTML) == "" {
		s.Templates.HTML = def.Templates.HTML
	}
	if strings.TrimSpace(s.Templates.Text) == "" {
		s.Templates.Text = def.Templates.Text
	}
	if s.Delivery.SettleMinutes < 0 {
		s.Delivery.SettleMinutes = 0
	}
	if s.Delivery.SettleMinutes > 240 {
		s.Delivery.SettleMinutes = 240
	}
	if s.Delivery.MaxItems <= 0 {
		s.Delivery.MaxItems = def.Delivery.MaxItems
	}
	if s.Delivery.MaxItems > 500 {
		s.Delivery.MaxItems = 500
	}
	if s.Delivery.MaxImages < 0 {
		s.Delivery.MaxImages = 0
	}
	if s.Delivery.MaxImages > 150 {
		s.Delivery.MaxImages = 150
	}
	if s.Delivery.SynopsisLength < 0 {
		s.Delivery.SynopsisLength = 0
	}
	s.Theme.BackdropDim = clamp(s.Theme.BackdropDim, 0, 95)
	s.Delivery.InstantGapMinutes = clamp(s.Delivery.InstantGapMinutes, 0, 1440)
	s.Delivery.InstantDailyCap = clamp(s.Delivery.InstantDailyCap, 0, 500)
	s.Delivery.BurstThreshold = clamp(s.Delivery.BurstThreshold, 0, 500)
	s.Delivery.QuietStart = clamp(s.Delivery.QuietStart, 0, 23)
	s.Delivery.QuietEnd = clamp(s.Delivery.QuietEnd, 0, 23)
	if s.Delivery.MaxAgeHours <= 0 {
		s.Delivery.MaxAgeHours = 168
	}
	s.Delivery.MaxAgeHours = clamp(s.Delivery.MaxAgeHours, 1, maxAgeLimit)
	s.Schedule.DailyHour = clamp(s.Schedule.DailyHour, 0, 23)
	s.Schedule.WeeklyHour = clamp(s.Schedule.WeeklyHour, 0, 23)
	s.Schedule.WeeklyDay = clamp(s.Schedule.WeeklyDay, 0, 6)
	cleaned := s.CustomVars[:0]
	for _, v := range s.CustomVars {
		v.Key = strings.TrimSpace(v.Key)
		if v.Key != "" {
			cleaned = append(cleaned, v)
		}
	}
	s.CustomVars = cleaned
}

func (s Settings) location() *time.Location {
	if s.Schedule.Timezone != "" {
		if loc, err := time.LoadLocation(s.Schedule.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
