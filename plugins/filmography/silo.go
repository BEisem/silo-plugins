package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("silo api returned %d: %s", e.Status, e.Detail)
}

// pacer spaces API requests. An API key shares its owner's rate allowance
// (20 requests a second on the standard tier) with their own sessions.
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

func (c *SiloClient) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		if err := pace(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.profile != "" {
			req.Header.Set("X-Profile-Id", c.profile)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			wait := 2 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = min(time.Duration(s)*time.Second, 30*time.Second)
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode >= 300 {
			var problem struct {
				Title  string `json:"title"`
				Detail string `json:"detail"`
			}
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
		if out == nil || len(body) == 0 {
			return nil
		}
		return json.Unmarshal(body, out)
	}
}

func (c *SiloClient) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// PrimaryProfile returns the API key owner's primary profile.
func (c *SiloClient) PrimaryProfile(ctx context.Context) (string, error) {
	var resp struct {
		Items []struct {
			ID        string `json:"id"`
			IsPrimary bool   `json:"is_primary"`
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
	if len(resp.Items) > 0 {
		return resp.Items[0].ID, nil
	}
	return "", errors.New("the API key's account has no profiles")
}

type siloPerson struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	TMDBID string `json:"tmdb_id"`
}

// findPerson returns Silo's person for a TMDB person id, or nil when the
// library has nobody with that id. People are searched by name.
func (c *SiloClient) findPerson(ctx context.Context, name, tmdbID string) (*siloPerson, error) {
	q := url.Values{}
	q.Set("q", name)
	q.Set("limit", "20")
	var resp struct {
		Items []siloPerson `json:"items"`
	}
	if err := c.get(ctx, "/api/v2/catalog/people", q, &resp); err != nil {
		return nil, err
	}
	for _, p := range resp.Items {
		if p.TMDBID == tmdbID {
			return &p, nil
		}
	}
	return nil, nil
}

func (c *SiloClient) person(ctx context.Context, id string) (*siloPerson, error) {
	q := url.Values{}
	q.Set("prefetch", "true")
	var p siloPerson
	if err := c.get(ctx, "/api/v2/catalog/people/"+url.PathEscape(id), q, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type libraryCard struct {
	ContentID string `json:"content_id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
}

// personTitles lists the library titles Silo credits a person on.
func (c *SiloClient) personTitles(ctx context.Context, personID string) ([]libraryCard, error) {
	var out []libraryCard
	cursor := ""
	for page := 0; page < 20; page++ {
		q := url.Values{}
		q.Set("source", "person")
		q.Set("person_id", personID)
		q.Set("limit", "200")
		q.Set("skip_total", "true")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Items []libraryCard `json:"items"`
			Page  struct {
				NextCursor string `json:"next_cursor"`
				HasMore    bool   `json:"has_more"`
			} `json:"page"`
		}
		if err := c.get(ctx, "/api/v2/catalog", q, &resp); err != nil {
			return out, err
		}
		out = append(out, resp.Items...)
		if !resp.Page.HasMore || resp.Page.NextCursor == "" {
			break
		}
		cursor = resp.Page.NextCursor
	}
	return out, nil
}

// itemTMDBID reads an item's TMDB id from its detail.
func (c *SiloClient) itemTMDBID(ctx context.Context, contentID string) (string, error) {
	var d struct {
		TMDBID string `json:"tmdb_id"`
	}
	if err := c.get(ctx, "/api/v2/catalog/items/"+url.PathEscape(contentID), nil, &d); err != nil {
		return "", err
	}
	return d.TMDBID, nil
}
