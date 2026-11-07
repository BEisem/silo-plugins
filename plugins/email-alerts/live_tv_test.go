//go:build live

package main

import (
	"context"
	"os"
	"testing"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func TestLiveTV(t *testing.T) {
	base, key := os.Getenv("SILO_URL"), os.Getenv("SILO_KEY")
	if base == "" || key == "" {
		t.Skip("set SILO_URL and SILO_KEY")
	}
	ctx := context.Background()
	a := newApp(hclog.NewNullLogger())
	a.cfg = Config{APIKey: key}
	a.store.backend = &memState{m: map[string][]byte{}}
	a.libs = []*pluginv1.Library{{Id: "1", Name: "Movies", MediaType: "movie"}, {Id: "2", Name: "Series", MediaType: "tv"}}
	client := a.client(ctx, endpoints{internal: base})
	for _, e := range a.newestEpisodes(ctx, client, 4) {
		t.Logf("newest: %s %s S%02dE%02d %q", e.Card.ContentID, e.Card.SeriesTitle, deref(e.Card.SeasonNumber), deref(e.Card.EpisodeNumber), e.Card.Title)
	}
	n, err := a.detectTV(ctx, client)
	t.Logf("baseline: n=%d err=%v", n, err)
	var st tvState
	a.store.read(ctx, keyTVState, &st)
	t.Logf("file watermark %d, %d shows tracked", st.FileWatermark, len(st.Seasons))
	n, err = a.detectTV(ctx, client)
	t.Logf("second pass: n=%d err=%v", n, err)
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
