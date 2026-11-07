package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

//go:embed web/filmography.html
var page []byte

type routeServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	app *App
}

var numericID = regexp.MustCompile(`^[0-9]{1,20}$`)

func (s *routeServer) Handle(ctx context.Context, in *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	path := in.GetPath()
	a := s.app
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
		p, err := a.person(ctx, id)
		if err != nil {
			return apiErr(err), nil
		}
		return jsonResp(200, p), nil

	case strings.HasPrefix(path, "/api/silo-person/"):
		id := strings.TrimPrefix(path, "/api/silo-person/")
		if !numericID.MatchString(id) {
			return errResp(400, "bad person id"), nil
		}
		tid, err := a.siloPersonTMDB(ctx, id)
		if err != nil {
			return apiErr(err), nil
		}
		return jsonResp(200, map[string]string{"tmdb_id": tid}), nil
	}
	return errResp(404, "not found"), nil
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
		return errResp(502, ae.Detail+" (check the plugin's API keys)")
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
