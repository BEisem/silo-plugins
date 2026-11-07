package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/webp"
)

// Card is the subset of a Silo catalog card the plugin reads.
type Card struct {
	ContentID              string   `json:"content_id"`
	Type                   string   `json:"type"`
	Title                  string   `json:"title"`
	Year                   int      `json:"year"`
	Overview               string   `json:"overview"`
	AddedAt                string   `json:"added_at"`
	ReleaseDate            string   `json:"release_date"`
	Runtime                int      `json:"runtime"`
	DurationSeconds        float64  `json:"duration_seconds"`
	ContentRating          string   `json:"content_rating"`
	Genres                 []string `json:"genres"`
	Studios                []string `json:"studios"`
	Networks               []string `json:"networks"`
	RatingTMDB             float64  `json:"rating_tmdb"`
	RatingIMDB             float64  `json:"rating_imdb"`
	PosterURL              string   `json:"poster_url"`
	BackdropURL            string   `json:"backdrop_url"`
	PosterIsEpisodeStill   bool     `json:"poster_is_episode_still"`
	BackdropIsEpisodeStill bool     `json:"backdrop_is_episode_still"`
	SeriesID               string   `json:"series_id"`
	SeriesTitle            string   `json:"series_title"`
	SeasonNumber           *int     `json:"season_number"`
	EpisodeNumber          *int     `json:"episode_number"`
	ShowStatus             string   `json:"show_status"`
}

// Detail is the subset of a catalog item detail the plugin reads.
type Detail struct {
	Card
	Tagline      string       `json:"tagline"`
	FirstAirDate string       `json:"first_air_date"`
	AirDate      string       `json:"air_date"`
	SeasonCount  int          `json:"season_count"`
	EpisodeCount int          `json:"episode_count"`
	LogoURL      string       `json:"logo_url"`
	Cast         []CastMember `json:"cast"`
}

type CastMember struct {
	Name      string `json:"name"`
	Character string `json:"character"`
}

func (c Card) added() time.Time {
	t, err := time.Parse(time.RFC3339Nano, c.AddedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

type SiloClient struct {
	base string
	key  string
	// profile is sent as X-Profile-Id: catalog routes need an acting
	// profile even for an admin API key.
	profile string
	http    *http.Client
}

func newSiloClient(base, key, profile string) *SiloClient {
	return &SiloClient{
		base:    strings.TrimRight(base, "/"),
		key:     key,
		profile: profile,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// PrimaryProfile returns the API key owner's primary profile, falling back
// to the first profile without a PIN or content limits.
func (c *SiloClient) PrimaryProfile(ctx context.Context) (string, error) {
	var resp struct {
		Items []struct {
			ID               string `json:"id"`
			IsPrimary        bool   `json:"is_primary"`
			HasPIN           bool   `json:"has_pin"`
			IsChild          bool   `json:"is_child"`
			MaxContentRating string `json:"max_content_rating"`
			MaxAdvisoryAge   *int   `json:"max_advisory_age"`
		} `json:"items"`
	}
	saved := c.profile
	c.profile = ""
	err := c.get(ctx, "/api/v2/profiles", nil, &resp)
	c.profile = saved
	if err != nil {
		return "", err
	}
	for _, p := range resp.Items {
		if p.IsPrimary {
			return p.ID, nil
		}
	}
	for _, p := range resp.Items {
		if !p.HasPIN && !p.IsChild && p.MaxContentRating == "" && p.MaxAdvisoryAge == nil {
			return p.ID, nil
		}
	}
	if len(resp.Items) > 0 {
		return resp.Items[0].ID, nil
	}
	return "", errors.New("the API key's account has no profiles")
}

type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("silo api returned %d: %s", e.Status, e.Detail)
}

// pacer spaces API requests across every client in the process. An API key
// shares its owner's rate allowance (20 requests a second on the standard
// tier) with their own sessions, so the plugin stays well under it.
var pacer = struct {
	sync.Mutex
	next time.Time
}{}

const requestGap = 125 * time.Millisecond

func pace(ctx context.Context) error {
	pacer.Lock()
	now := time.Now()
	at := pacer.next
	if at.Before(now) {
		at = now
	}
	pacer.next = at.Add(requestGap)
	pacer.Unlock()
	return sleepCtx(ctx, time.Until(at))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *SiloClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var (
		resp *http.Response
		body []byte
	)
	for attempt := 0; ; attempt++ {
		if err := pace(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		if c.profile != "" {
			req.Header.Set("X-Profile-Id", c.profile)
		}
		resp, err = c.http.Do(req)
		if err != nil {
			return err
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= 3 {
			break
		}
		wait := 2 * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
	if resp.StatusCode >= 300 {
		var problem struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		// Not every error body is a problem document; an empty title falls
		// back to the raw body below.
		_ = json.Unmarshal(body, &problem)
		detail := strings.TrimSpace(problem.Title + ": " + problem.Detail)
		if problem.Title == "" {
			detail = strings.TrimSpace(string(body))
			if len(detail) > 200 {
				detail = detail[:200]
			}
		}
		return &apiError{Status: resp.StatusCode, Detail: detail}
	}
	return json.Unmarshal(body, out)
}

const (
	posterWidth   = "342"
	backdropWidth = "780"
	stillWidth    = "640"
)

func imageQuery(q url.Values) url.Values {
	q.Set("poster_width", posterWidth)
	q.Set("backdrop_width", backdropWidth)
	q.Set("still_width", stillWidth)
	return q
}

// Recent lists items of one type in one library, newest first, stopping
// once items are older than since or limit items have been read.
func (c *SiloClient) Recent(ctx context.Context, libraryID, itemType string, since time.Time, limit int) ([]Card, error) {
	var out []Card
	cursor := ""
	for page := 0; page < 20; page++ {
		q := imageQuery(url.Values{})
		q.Set("type", itemType)
		q.Set("sort", "-added_at")
		q.Set("limit", "100")
		q.Set("skip_total", "true")
		if libraryID != "" {
			q.Set("library_id", libraryID)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Items []Card `json:"items"`
			Page  struct {
				NextCursor string `json:"next_cursor"`
				HasMore    bool   `json:"has_more"`
			} `json:"page"`
		}
		if err := c.get(ctx, "/api/v2/catalog", q, &resp); err != nil {
			return out, err
		}
		for _, card := range resp.Items {
			added := card.added()
			if !since.IsZero() && !added.IsZero() && !added.After(since) {
				return out, nil
			}
			out = append(out, card)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
		if !resp.Page.HasMore || resp.Page.NextCursor == "" {
			return out, nil
		}
		cursor = resp.Page.NextCursor
	}
	return out, nil
}

func (c *SiloClient) Item(ctx context.Context, id string) (*Detail, error) {
	var d Detail
	if err := c.get(ctx, "/api/v2/catalog/items/"+url.PathEscape(id), imageQuery(url.Values{}), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Check confirms the API key works and returns a friendly error otherwise.
func (c *SiloClient) Check(ctx context.Context) error {
	var resp map[string]any
	q := url.Values{"limit": {"1"}, "skip_total": {"true"}}
	err := c.get(ctx, "/api/v2/catalog", q, &resp)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
		return fmt.Errorf("the Silo API key was refused (%d); create a new unscoped key on an admin account", ae.Status)
	}
	return err
}

// Image downloads one artwork URL. Silo returns presigned, short-lived URLs
// that can be relative to the server, so they are embedded in the email
// rather than linked.
func (c *SiloClient) Image(ctx context.Context, raw string) ([]byte, string, error) {
	if raw == "" {
		return nil, "", errors.New("empty image url")
	}
	u := raw
	if strings.HasPrefix(raw, "/") {
		u = c.base + raw
	}
	sameHost := sameHost(c.base, raw)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	if sameHost {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("image returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	if ct == "" || ct == "application/octet-stream" {
		ct = http.DetectContentType(data)
	}
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("not an image (%s)", ct)
	}
	if ct == "image/webp" {
		// Silo serves artwork as WebP, which Outlook doesn't display.
		if jpg, err := webpToJPEG(data); err == nil {
			return jpg, "image/jpeg", nil
		}
	}
	return data, ct, nil
}

func webpToJPEG(data []byte) ([]byte, error) {
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// libraryTypes decides which catalog types to query for a library.
func libraryTypes(mediaType string) []string {
	mt := strings.ToLower(mediaType)
	switch {
	case strings.Contains(mt, "movie") || strings.Contains(mt, "film"):
		return []string{"movie"}
	case strings.Contains(mt, "tv") || strings.Contains(mt, "show") || strings.Contains(mt, "series") || strings.Contains(mt, "episode"):
		return []string{"episode"}
	case strings.Contains(mt, "music"), strings.Contains(mt, "audio"), strings.Contains(mt, "book"),
		strings.Contains(mt, "manga"), strings.Contains(mt, "podcast"), strings.Contains(mt, "photo"):
		return nil
	default:
		return []string{"movie", "episode"}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// sameHost reports whether raw points at the Silo server at base, so the API
// key is only ever sent there. A plain prefix check would also match
// "https://silo.example.com.evil.test/", so the host is compared exactly.
func sameHost(base, raw string) bool {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return true
	}
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.User == nil && strings.EqualFold(u.Scheme, b.Scheme) && strings.EqualFold(u.Host, b.Host)
}
