package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type memState struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (m *memState) ReadInstanceState(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	return v, ok, nil
}

func (m *memState) WriteInstanceState(ctx context.Context, key string, value []byte) error {
	// Like the real host, refuse writes once the caller's context is done.
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = value
	return nil
}

// fakeSilo serves the few routes the detector reads. episodes maps
// "series/season" to episode id to file id.
type fakeSilo struct {
	mu       sync.Mutex
	order    []string
	episodes map[string]map[string]int
}

func (f *fakeSilo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/api/v2/library/2/sections":
		write(map[string]any{"sections": []map[string]any{{"id": "ra", "section_type": "recently_added"}}})
	case p == "/api/v2/library/2/sections/ra/items":
		var items []map[string]any
		for _, id := range f.order {
			// "ep:<show>" is a show's single new episode, which Silo lists
			// as an episode card.
			if show, ok := strings.CutPrefix(id, "ep:"); ok {
				items = append(items, map[string]any{"content_id": "episode-" + show, "type": "episode", "series_id": show, "series_title": show})
				continue
			}
			items = append(items, map[string]any{"content_id": id, "type": "series", "title": id})
		}
		write(map[string]any{"items": items})
	case strings.HasSuffix(p, "/seasons"):
		series := strings.Split(p, "/")[5]
		var items []map[string]any
		for key, eps := range f.episodes {
			parts := strings.Split(key, "/")
			if parts[0] == series {
				var n int
				fmt.Sscanf(parts[1], "%d", &n)
				items = append(items, map[string]any{"season_number": n, "episode_count": len(eps)})
			}
		}
		write(map[string]any{"items": items})
	case strings.HasSuffix(p, "/episodes"):
		parts := strings.Split(p, "/")
		var items []map[string]any
		for id, file := range f.episodes[parts[5]+"/"+parts[7]] {
			items = append(items, map[string]any{"content_id": id, "files": []map[string]any{{"file_id": fmt.Sprint(file)}}})
		}
		write(map[string]any{"items": items})
	default:
		http.NotFound(w, r)
	}
}

func TestDetectTVBaselineThenArrivalsAndUpgrades(t *testing.T) {
	fake := &fakeSilo{
		order: []string{"showA", "showB"},
		episodes: map[string]map[string]int{
			"showA/1": {"a-1-1": 100, "a-1-2": 101},
			"showB/2": {"b-2-1": 90},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	a := newApp(hclog.NewNullLogger())
	a.store.backend = &memState{m: map[string][]byte{}}
	a.libs = []*pluginv1.Library{{Id: "2", Name: "Series", MediaType: "tv"}}
	client := newSiloClient(srv.URL, "k", "p")
	ctx := context.Background()

	// First run is a baseline: nothing already in the library is announced.
	if n, err := a.detectTV(ctx, client); err != nil || n != 0 {
		t.Fatalf("baseline: n=%d err=%v", n, err)
	}

	// A new episode of an existing show, whose item existed long before.
	fake.mu.Lock()
	fake.episodes["showB/2"]["b-2-2"] = 150
	fake.order = []string{"showB", "showA"}
	fake.mu.Unlock()
	if n, err := a.detectTV(ctx, client); err != nil || n != 1 {
		t.Fatalf("new episode: n=%d err=%v", n, err)
	}

	// An upgrade swaps a file without changing the count: not announced.
	fake.mu.Lock()
	fake.episodes["showA/1"]["a-1-1"] = 160
	fake.mu.Unlock()
	if n, err := a.detectTV(ctx, client); err != nil || n != 0 {
		t.Fatalf("upgrade: n=%d err=%v", n, err)
	}

	// A brand-new show, and a new season of an existing one.
	fake.mu.Lock()
	fake.episodes["showC/1"] = map[string]int{"c-1-1": 170, "c-1-2": 171}
	fake.episodes["showA/2"] = map[string]int{"a-2-1": 172}
	fake.order = []string{"showC", "showA", "showB"}
	fake.mu.Unlock()
	if n, err := a.detectTV(ctx, client); err != nil || n != 3 {
		t.Fatalf("new show and season: n=%d err=%v", n, err)
	}

	list, err := a.arrivals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range list {
		got[x.ID] = true
	}
	for _, want := range []string{"b-2-2", "c-1-1", "c-1-2", "a-2-1"} {
		if !got[want] {
			t.Errorf("arrival %s missing; have %v", want, got)
		}
	}
	if got["a-1-1"] {
		t.Error("upgraded episode was recorded as an arrival")
	}

	// Running again with nothing new records nothing.
	if n, err := a.detectTV(ctx, client); err != nil || n != 0 {
		t.Fatalf("repeat: n=%d err=%v", n, err)
	}

	// One new episode of a show: the row shows the episode, not the show.
	fake.mu.Lock()
	fake.episodes["showB/2"]["b-2-3"] = 180
	fake.order = []string{"ep:showB", "showC", "showA"}
	fake.mu.Unlock()
	if n, err := a.detectTV(ctx, client); err != nil || n != 1 {
		t.Fatalf("episode card: n=%d err=%v", n, err)
	}
}

func TestRowSeries(t *testing.T) {
	cards := []Card{
		{ContentID: "episode-tvdb-1-2-3", Type: "episode", SeriesID: "series-tvdb-1", SeriesTitle: "One"},
		{ContentID: "episode-tvdb-2-1-1", Type: "episode", SeriesID: "series-tvdb-2"},
		{ContentID: "series-tvdb-1", Type: "series"},
		{ContentID: "episode-orphan", Type: "episode"},
		{ContentID: "series-tvdb-3", Type: "series"},
		{ContentID: "series-tvdb-4", Type: "series"},
	}
	var got []string
	for _, c := range rowSeries(cards, 3) {
		got = append(got, c.ContentID)
	}
	if want := "series-tvdb-1 series-tvdb-2 series-tvdb-3"; strings.Join(got, " ") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}
