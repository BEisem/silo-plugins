package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

//go:embed web/filmography.html
var pageTemplate []byte

// page is the served page, with its Content-Security-Policy filled in.
var page = withCSP(pageTemplate)

// The page shares Silo's origin, so a script injected into it would run as
// Silo. Its Content-Security-Policy allows only the page's own script (by
// hash, so injected <script> tags and inline handlers don't run), requests
// to its own origin, and images from TMDB. The policy goes in a <meta> tag
// because Silo passes only a few response headers through from plugins.
func withCSP(tmpl []byte) []byte {
	html := string(tmpl)
	start := strings.Index(html, "<script>")
	end := strings.Index(html, "</script>")
	if start < 0 || end < start || strings.Count(html, "<script") != 1 || !strings.Contains(html, "__CSP__") {
		panic("filmography.html must hold one <script> block and a __CSP__ placeholder")
	}
	sum := sha256.Sum256([]byte(html[start+len("<script>") : end]))
	csp := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src 'unsafe-inline'; img-src https://image.tmdb.org; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'"
	return []byte(strings.Replace(html, "__CSP__", csp, 1))
}

type routeServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	app *App
}

var numericID = regexp.MustCompile(`^[0-9]{1,20}$`)

func (s *routeServer) Handle(ctx context.Context, in *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	path := in.GetPath()
	a := s.app
	if in.GetMethod() != "" && in.GetMethod() != "GET" {
		return errResp(405, "method not allowed"), nil
	}
	switch {
	case path == "/filmography/" || path == "/filmography":
		return &pluginv1.HandleHTTPResponse{StatusCode: 200, Headers: map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"}, Body: page}, nil

	case path == "/api/search":
		q := strings.TrimSpace(queryValue(in, "q"))
		if len(q) > 100 {
			return errResp(400, "search text is too long"), nil
		}
		res, err := a.search(ctx, q)
		if err != nil {
			return apiErr(err), nil
		}
		return jsonResp(200, map[string]any{"results": res, "popular": q == ""}), nil

	case strings.HasPrefix(path, "/api/person/"):
		id := strings.TrimPrefix(path, "/api/person/")
		if !numericID.MatchString(id) {
			return errResp(400, "bad person id"), nil
		}
		p, err := a.person(ctx, id, header(in, "X-Silo-User-Id"))
		if err != nil {
			return apiErr(err), nil
		}
		return jsonResp(200, p), nil
	}
	return errResp(404, "not found"), nil
}

// header reads a request header by name, ignoring case. Silo forwards only
// a short allowlist of browser headers and sets the X-Silo-* identity
// headers itself, so a browser can't supply its own X-Silo-User-Id.
func header(in *pluginv1.HandleHTTPRequest, name string) string {
	for k, v := range in.GetHeaders() {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// queryValue reads one query parameter, which Silo passes as a string or
// a list of strings.
func queryValue(in *pluginv1.HandleHTTPRequest, key string) string {
	if in.GetQuery() == nil {
		return ""
	}
	switch v := in.GetQuery().AsMap()[key].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return s
		}
	}
	return ""
}

func apiErr(err error) *pluginv1.HandleHTTPResponse {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 404 {
		return errResp(404, "not found")
	}
	if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
		return errResp(502, ae.Detail+" (check the plugin's TMDB API key)")
	}
	return errResp(502, err.Error())
}

func jsonResp(status int, v any) *pluginv1.HandleHTTPResponse {
	body, _ := json.Marshal(v)
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       body,
	}
}

func errResp(status int, msg string) *pluginv1.HandleHTTPResponse {
	return jsonResp(status, map[string]string{"error": msg})
}
