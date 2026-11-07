package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func intp(i int) *int { return &i }

func TestBuildPersonMergesRoles(t *testing.T) {
	p := &tmdbPerson{ID: 31, Name: "Tom Hanks", ProfilePath: "/b.jpg"}
	p.Images.Profiles = append(p.Images.Profiles,
		struct {
			FilePath string  `json:"file_path"`
			Width    int     `json:"width"`
			Height   int     `json:"height"`
			Votes    float64 `json:"vote_average"`
		}{FilePath: "/a.jpg", Width: 400, Height: 600},
		struct {
			FilePath string  `json:"file_path"`
			Width    int     `json:"width"`
			Height   int     `json:"height"`
			Votes    float64 `json:"vote_average"`
		}{FilePath: "/b.jpg", Width: 400, Height: 600})
	p.ExternalIDs.IMDb = "nm0000158"
	p.ExternalIDs.Wikidata = "Q2263"
	p.ExternalIDs.Instagram = "tomhanks"
	p.Credits.Cast = []tmdbCredit{
		{ID: 13, MediaType: "movie", Title: "Forrest Gump", ReleaseDate: "1994-06-23", Character: "Forrest Gump", Order: intp(0)},
		{ID: 1, MediaType: "tv", Name: "The Tonight Show", Character: "Self", GenreIDs: []int{10767}, EpisodeCount: 12},
		{ID: 2, MediaType: "tv", Name: "Band of Brothers", Character: "Narrator", Order: intp(3)},
		{ID: 3, MediaType: "movie", Title: "Doc", Character: "Himself - Actor"},
		{ID: 4, MediaType: "movie", Title: "Adult", Adult: true},
		{ID: 5, MediaType: "movie", Title: "Bloody Hundredth", Character: "Self - Narrator (voice)"},
	}
	p.Credits.Crew = []tmdbCredit{
		{ID: 2, MediaType: "tv", Name: "Band of Brothers", Job: "Executive Producer", EpisodeCount: 10},
		{ID: 2, MediaType: "tv", Name: "Band of Brothers", Job: "Executive Producer", EpisodeCount: 10},
		{ID: 2, MediaType: "tv", Name: "Band of Brothers", Job: "Director", EpisodeCount: 1},
		{ID: 5, MediaType: "movie", Title: "Bloody Hundredth", Job: "Executive Producer"},
	}
	got := buildPerson(p, "en")
	if len(got.Credits) != 5 {
		t.Fatalf("credits = %d, want 5 (adult dropped, duplicates merged)", len(got.Credits))
	}
	byID := map[int]Credit{}
	for _, c := range got.Credits {
		byID[c.ID] = c
	}
	if c := byID[1]; !c.Self {
		t.Errorf("talk show should be a self appearance")
	}
	if c := byID[3]; !c.Self {
		t.Errorf("\"Himself - Actor\" should be a self appearance")
	}
	if c := byID[5]; !c.Self || !c.Crew {
		t.Errorf("a producer credit must not hide a self appearance: %+v", c)
	}
	bob := byID[2]
	if bob.Self || !bob.Acting || !bob.Crew || len(bob.Roles) != 3 || bob.Billing != 3 {
		t.Errorf("Band of Brothers = %+v", bob)
	}
	if byID[13].Billing != 0 || byID[13].Date != "1994-06-23" {
		t.Errorf("Forrest Gump = %+v", byID[13])
	}
	if got.Images[0].Path != "/b.jpg" {
		t.Errorf("current headshot should be first, got %v", got.Images)
	}
	kinds := []string{}
	for _, l := range got.Links {
		kinds = append(kinds, l.Kind)
	}
	if strings.Join(kinds, ",") != "tmdb,imdb,wikipedia,wikidata,instagram" {
		t.Errorf("links = %v", kinds)
	}
	if got.Links[2].URL != "https://www.wikidata.org/wiki/Special:GoToLinkedPage/enwiki/Q2263" {
		t.Errorf("wikipedia link = %s", got.Links[2].URL)
	}
}

// fakeServers stands in for TMDB and Silo.
func fakeServers(t *testing.T) (*App, func()) {
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "k" {
			w.WriteHeader(401)
			w.Write([]byte(`{"status_message":"Invalid API key"}`))
			return
		}
		switch r.URL.Path {
		case "/person/31":
			w.Write([]byte(`{"id":31,"name":"Tom Hanks","combined_credits":{"cast":[
				{"id":862,"media_type":"movie","title":"Toy Story","character":"Woody (voice)"},
				{"id":46518,"media_type":"tv","name":"Masters of the Air","character":""},
				{"id":13,"media_type":"movie","title":"Forrest Gump","character":"Forrest"}],"crew":[]},
				"images":{"profiles":[]},"external_ids":{}}`))
		case "/search/person":
			w.Write([]byte(`{"results":[{"id":31,"name":"Tom Hanks","known_for":[{"title":"Big"}]},{"id":9,"name":"X","adult":true}]}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"status_message":"not found"}`))
		}
	}))
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa_x" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/api/v2/catalog/people":
			w.Write([]byte(`{"items":[{"id":"55","name":"Tom Hanks Jr","tmdb_id":"999"},{"id":"77","name":"Tom Hanks","tmdb_id":"31"}]}`))
		case r.URL.Path == "/api/v2/catalog/people/77":
			w.Write([]byte(`{"id":"77","name":"Tom Hanks","tmdb_id":"31"}`))
		case r.URL.Path == "/api/v2/catalog" && r.URL.Query().Get("person_id") == "77":
			w.Write([]byte(`{"items":[{"content_id":"movie-tmdb-862","type":"movie"},{"content_id":"series-tvdb-370853","type":"series"}],"page":{"has_more":false}}`))
		case r.URL.Path == "/api/v2/catalog/items/series-tvdb-370853":
			w.Write([]byte(`{"tmdb_id":"46518"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	a := newApp(hclog.NewNullLogger())
	a.tmdbBase = tmdb.URL
	a.configure(entries(map[string]map[string]any{
		"tmdb": {"api_key": "k"},
		"silo": {"api_key": "sa_x", "profile_id": "p", "base_url": silo.URL},
	}))
	return a, func() { tmdb.Close(); silo.Close() }
}

func entries(m map[string]map[string]any) []*pluginv1.ConfigEntry {
	var out []*pluginv1.ConfigEntry
	for k, v := range m {
		s, _ := structpb.NewStruct(v)
		out = append(out, &pluginv1.ConfigEntry{Key: k, Value: s})
	}
	return out
}

func TestPersonMarksLibrary(t *testing.T) {
	a, done := fakeServers(t)
	defer done()
	p, err := a.person(context.Background(), "31")
	if err != nil {
		t.Fatal(err)
	}
	if p.SiloPersonID != "77" || p.LibraryError != "" {
		t.Fatalf("silo person = %q, error %q", p.SiloPersonID, p.LibraryError)
	}
	want := map[int]string{862: "movie-tmdb-862", 46518: "series-tvdb-370853", 13: ""}
	for _, c := range p.Credits {
		if c.ContentID != want[c.ID] {
			t.Errorf("%s content id = %q, want %q", c.Title, c.ContentID, want[c.ID])
		}
	}
	// The cached TMDB answer must not carry library marks.
	v, _ := a.cache.get("person:31")
	for _, c := range v.(*Person).Credits {
		if c.ContentID != "" {
			t.Errorf("cached credit %s was marked", c.Title)
		}
	}
}

func TestPersonWithoutSiloKeyStillWorks(t *testing.T) {
	a, done := fakeServers(t)
	defer done()
	a.configure(entries(map[string]map[string]any{"tmdb": {"api_key": "k"}}))
	p, err := a.person(context.Background(), "31")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Credits) != 3 || !strings.Contains(p.LibraryError, "no Silo API key") {
		t.Fatalf("credits %d, error %q", len(p.Credits), p.LibraryError)
	}
}

func TestRoutes(t *testing.T) {
	a, done := fakeServers(t)
	defer done()
	s := &routeServer{app: a}
	call := func(path string, q map[string]any) *pluginv1.HandleHTTPResponse {
		st, _ := structpb.NewStruct(q)
		r, err := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: path, Query: st})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := call("/filmography/", nil); r.StatusCode != 200 || !strings.Contains(string(r.Body), "<title>Filmography</title>") {
		t.Errorf("page = %d", r.StatusCode)
	}
	r := call("/api/search", map[string]any{"q": []any{"tom"}})
	if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"Tom Hanks"`) || strings.Contains(string(r.Body), `"X"`) {
		t.Errorf("search = %d %s", r.StatusCode, r.Body)
	}
	if r := call("/api/person/31", nil); r.StatusCode != 200 {
		t.Errorf("person = %d %s", r.StatusCode, r.Body)
	}
	if r := call("/api/person/../x", nil); r.StatusCode != 400 {
		t.Errorf("bad id = %d", r.StatusCode)
	}
	if r := call("/api/person/4040", nil); r.StatusCode != 404 {
		t.Errorf("unknown person = %d %s", r.StatusCode, r.Body)
	}
	if r := call("/api/silo-person/77", nil); r.StatusCode != 200 || !strings.Contains(string(r.Body), `"31"`) {
		t.Errorf("silo person = %d %s", r.StatusCode, r.Body)
	}
	a.configure(entries(map[string]map[string]any{"tmdb": {"api_key": "wrong"}}))
	if r := call("/api/person/31", nil); r.StatusCode != 502 || !strings.Contains(string(r.Body), "Invalid API key") {
		t.Errorf("bad key = %d %s", r.StatusCode, r.Body)
	}
}
