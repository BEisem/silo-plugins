package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errSiloUnavailable marks failures to reach Silo's own API.
var errSiloUnavailable = errors.New("silo unreachable")

// siloClient looks up Silo accounts through the admin users API so that anyone
// with an enabled Silo account (including a newly accepted invitation) may
// sign in with Google.
type siloClient struct {
	base string
	key  string
	hc   *http.Client
}

func newSiloClient(base, key string, hc *http.Client) *siloClient {
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return &siloClient{base: strings.TrimRight(base, "/"), key: key, hc: hc}
}

// accountByEmail reports whether an enabled Silo account has this email.
func (c *siloClient) accountByEmail(ctx context.Context, email string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := c.base + "/api/v2/admin/users?limit=10&identity=" + url.QueryEscape(email)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errSiloUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("silo users API answered %d", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			Email   string `json:"email"`
			Enabled bool   `json:"enabled"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("unreadable users response: %v", err)
	}
	for _, it := range out.Items {
		if it.Enabled && strings.EqualFold(strings.TrimSpace(it.Email), strings.TrimSpace(email)) {
			return true, nil
		}
	}
	return false, nil
}
