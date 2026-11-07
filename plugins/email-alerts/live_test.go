//go:build live

package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

// TestLiveRender renders a real email from a running Silo server:
//
//	SILO_URL=http://localhost:8098 SILO_KEY=sa_... OUT_DIR=/tmp/out go test -tags live -run TestLiveRender -v
func TestLiveRender(t *testing.T) {
	base, key, out := os.Getenv("SILO_URL"), os.Getenv("SILO_KEY"), os.Getenv("OUT_DIR")
	if base == "" || key == "" || out == "" {
		t.Skip("set SILO_URL, SILO_KEY and OUT_DIR")
	}
	ctx := context.Background()
	a := newApp(hclog.NewNullLogger())
	a.cfg = Config{APIKey: key, ServerName: "Silo"}
	ep := endpoints{internal: base, public: "https://silo.example.com"}
	client := a.client(ctx, ep)
	if client.profile == "" {
		t.Fatal("no profile resolved")
	}
	entries, err := a.latest(ctx, client, 3)
	if err != nil {
		t.Logf("collect warning: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries from the catalog")
	}
	for _, e := range entries {
		still := ""
		if e.Episode != nil && e.Episode.PosterIsEpisodeStill {
			still = "still"
		}
		show := ""
		if e.Series != nil {
			show = e.Series.Overview
			if len(show) > 40 {
				show = show[:40]
			}
		}
		t.Logf("%s %q series=%q show_synopsis=%q %s", e.Card.Type, e.Card.Title, e.Card.SeriesTitle, show, still)
	}
	env := RenderEnv{Settings: defaultSettings(), ServerName: "Silo", PublicURL: ep.public, InternalBase: base, Mode: "instant", Now: time.Now(),
		Recipient: Recipient{Name: "Pete"}}
	r, err := Render(ctx, env, entries, client.Image)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("subject: %s", r.Subject)
	t.Logf("embedded %d images", len(r.Images))
	html := r.HTML
	for _, img := range r.Images {
		if img.ContentType != "image/jpeg" {
			t.Errorf("image %s is %s, want image/jpeg", img.CID, img.ContentType)
		}
		html = strings.ReplaceAll(html, "cid:"+img.CID, "data:"+img.ContentType+";base64,"+base64.StdEncoding.EncodeToString(img.Data))
	}
	if err := os.WriteFile(filepath.Join(out, "live-email.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "live-email.txt"), []byte(r.Subject+"\n\n"+r.Text), 0o644); err != nil {
		t.Fatal(err)
	}
}
