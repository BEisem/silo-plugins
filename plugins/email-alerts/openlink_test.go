package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLinksGoThroughOpenPage(t *testing.T) {
	env := RenderEnv{Settings: defaultSettings(), ServerName: "Silo", PublicURL: "https://silo.example.com",
		PluginBase: "https://silo.example.com/api/v2/plugin-content/plugins/5", Mode: "instant", Now: time.Now()}
	r, err := Render(context.Background(), env, sampleEntries(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`href="https://silo.example.com/api/v2/plugin-content/plugins/5/public/open?id=episode%3Asample-1"`,
		`href="https://silo.example.com/api/v2/plugin-content/plugins/5/public/open?id=series%3Asample-harbour-lights"`,
		`href="https://silo.example.com/api/v2/plugin-content/plugins/5/public/open?id=movie%3Asample-3"`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %s", want)
		}
	}

	env.Settings.Theme.OpenInApp = false
	r, err = Render(context.Background(), env, sampleEntries(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "/public/open") || !strings.Contains(r.HTML, `href="https://silo.example.com/item/episode:sample-1"`) {
		t.Fatal("with the switch off, links should go straight to the web")
	}
}

func TestContentIDPattern(t *testing.T) {
	for _, ok := range []string{"episode-tvdb-426606-1-1", "movie-tmdb-9902", "series:sample"} {
		if !contentIDPattern.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "a/b", "x\"><script>", "javascript:alert(1) ", strings.Repeat("a", 201)} {
		if contentIDPattern.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
