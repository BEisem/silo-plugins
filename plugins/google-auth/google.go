package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// endpoints are Google's OAuth URLs; tests point them at a fake server.
type endpoints struct {
	Auth      string
	Token     string
	Discovery string
}

var googleEndpoints = endpoints{
	Auth:      "https://accounts.google.com/o/oauth2/v2/auth",
	Token:     "https://oauth2.googleapis.com/token",
	Discovery: "https://accounts.google.com/.well-known/openid-configuration",
}

// idClaims are the ID token claims the plugin uses.
type idClaims struct {
	Issuer        string `json:"iss"`
	Audience      string `json:"aud"`
	Subject       string `json:"sub"`
	Expires       int64  `json:"exp"`
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// errTransport marks failures to reach Google (as opposed to Google refusing).
var errTransport = errors.New("google unreachable")

// validPrompts are the OIDC prompt values Google accepts.
var validPrompts = map[string]bool{"none": true, "consent": true, "select_account": true, "login": true}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeURL(ep endpoints, c config, redirect, state, challenge, nonce, prompt, hint string) string {
	q := url.Values{
		"client_id":             {c.ClientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"nonce":                 {nonce},
		"access_type":           {"offline"},
	}
	if prompt == "" {
		prompt = c.Prompt
	}
	if validPrompts[prompt] {
		q.Set("prompt", prompt)
	}
	if hint != "" {
		q.Set("login_hint", hint)
	}
	return ep.Auth + "?" + q.Encode()
}

// postToken calls the token endpoint. A non-2xx answer with a JSON error body
// is returned as a tokenResponse with Error set, not as a Go error.
func postToken(ctx context.Context, hc *http.Client, ep endpoints, form url.Values) (tokenResponse, error) {
	var tr tokenResponse
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return tr, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return tr, fmt.Errorf("%w: %v", errTransport, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tr, fmt.Errorf("%w: %v", errTransport, err)
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		if resp.StatusCode >= 500 {
			return tr, fmt.Errorf("%w: status %d", errTransport, resp.StatusCode)
		}
		return tr, fmt.Errorf("unreadable token response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return tr, fmt.Errorf("%w: status %d", errTransport, resp.StatusCode)
	}
	return tr, nil
}

// parseIDToken decodes the claims and checks issuer, audience, expiry and
// (when wantNonce is non-empty) nonce. The signature is not verified: the token
// came straight from Google's token endpoint over TLS in exchange for our
// client secret, which OpenID Connect Core 3.1.3.7 accepts in place of
// signature validation.
func parseIDToken(tok, clientID, wantNonce string, now time.Time) (idClaims, error) {
	var c idClaims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, errors.New("id_token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, errors.New("id_token payload is not base64url")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, errors.New("id_token payload is not JSON")
	}
	if c.Issuer != "https://accounts.google.com" && c.Issuer != "accounts.google.com" {
		return c, fmt.Errorf("unexpected issuer %q", c.Issuer)
	}
	if c.Audience != clientID {
		return c, errors.New("id_token audience is not this client")
	}
	if c.Expires <= now.Unix() {
		return c, errors.New("id_token expired")
	}
	if c.Subject == "" {
		return c, errors.New("id_token has no subject")
	}
	if wantNonce != "" && c.Nonce != wantNonce {
		return c, errors.New("id_token nonce mismatch")
	}
	return c, nil
}
