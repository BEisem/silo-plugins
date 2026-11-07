package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
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

// fakeHost stands in for Silo's plugin host.
type fakeHost struct {
	// libraries maps a user id to the library ids that user may see.
	libraries map[string][]string
	// present maps "movie:<tmdb id>" / "tv:<tmdb id>" to where Silo has it.
	present map[string]runtimehost.MediaPresence
	batches []int
	userIDs []string
}

func (f *fakeHost) ListLibraries(_ context.Context, userID string) ([]*pluginv1.Library, error) {
	f.userIDs = append(f.userIDs, userID)
	var out []*pluginv1.Library
	for _, id := range f.libraries[userID] {
		out = append(out, &pluginv1.Library{Id: id})
	}
	return out, nil
}

func (f *fakeHost) CheckMediaPresence(_ context.Context, provider, mediaType string, ids []string) (map[string]runtimehost.MediaPresence, error) {
	if provider != "tmdb" || len(ids) > presenceBatch {
		return nil, errors.New("bad presence call")
	}
	f.batches = append(f.batches, len(ids))
	out := map[string]runtimehost.MediaPresence{}
	for _, id := range ids {
		if m, ok := f.present[mediaType+":"+id]; ok {
			out[id] = m
		}
	}
	return out, nil
}

// fakeTMDB serves Tom Hanks, a search, and 404s.
func fakeTMDB(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)
	return srv
}

func testApp(t *testing.T, host libraryHost) *App {
	a := newApp(hclog.NewNullLogger())
	a.tmdbBase = fakeTMDB(t).URL
	a.host = func() libraryHost { return host }
	a.configure(entries(map[string]map[string]any{"tmdb": {"api_key": "k"}}))
	return a
}

func defaultHost() *fakeHost {
	return &fakeHost{
		libraries: map[string][]string{"7": {"1"}, "8": {"1", "2"}},
		present: map[string]runtimehost.MediaPresence{
			"movie:862": {ExternalID: "862", MediaID: "movie-tmdb-862", LibraryID: "1"},
			"tv:46518":  {ExternalID: "46518", MediaID: "series-tvdb-370853", LibraryID: "2"},
		},
	}
}

func entries(m map[string]map[string]any) []*pluginv1.ConfigEntry {
	var out []*pluginv1.ConfigEntry
	for k, v := range m {
		s, _ := structpb.NewStruct(v)
		out = append(out, &pluginv1.ConfigEntry{Key: k, Value: s})
	}
	return out
}

func contentIDs(p *Person) map[int]string {
	out := map[int]string{}
	for _, c := range p.Credits {
		out[c.ID] = c.ContentID
	}
	return out
}

func TestPersonMarksOnlyLibrariesTheViewerSees(t *testing.T) {
	host := defaultHost()
	a := testApp(t, host)
	for _, tc := range []struct {
		user string
		want map[int]string
	}{
		// User 7 can't see library 2, so the series there isn't marked.
		{"7", map[int]string{862: "movie-tmdb-862", 46518: "", 13: ""}},
		{"8", map[int]string{862: "movie-tmdb-862", 46518: "series-tvdb-370853", 13: ""}},
		// A user with no libraries sees nothing marked.
		{"9", map[int]string{862: "", 46518: "", 13: ""}},
	} {
		p, err := a.person(context.Background(), "31", tc.user)
		if err != nil {
			t.Fatal(err)
		}
		if p.LibraryError != "" {
			t.Fatalf("user %s: library error %q", tc.user, p.LibraryError)
		}
		for id, want := range tc.want {
			if got := contentIDs(p)[id]; got != want {
				t.Errorf("user %s: title %d content id = %q, want %q", tc.user, id, got, want)
			}
		}
	}
	// Presence is shared between viewers; library lists are per viewer.
	if len(host.batches) != 2 {
		t.Errorf("presence calls = %v, want one movie and one tv call", host.batches)
	}
	// The cached TMDB answer must not carry library marks.
	v, _ := a.cache.get("person:31")
	for _, c := range v.(*Person).Credits {
		if c.ContentID != "" {
			t.Errorf("cached credit %s was marked", c.Title)
		}
	}
}

func TestPersonFailsClosedWithoutAViewer(t *testing.T) {
	host := defaultHost()
	for _, tc := range []struct {
		name string
		user string
		host libraryHost
		want string
	}{
		{"no user", "", host, "who is signed in"},
		{"bad user", "7 OR 1", host, "who is signed in"},
		{"zero user", "0", host, "who is signed in"},
		{"host not connected", "7", nil, "not connected"},
	} {
		a := testApp(t, tc.host)
		p, err := a.person(context.Background(), "31", tc.user)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Credits) != 3 || !strings.Contains(p.LibraryError, tc.want) {
			t.Errorf("%s: credits %d, error %q", tc.name, len(p.Credits), p.LibraryError)
		}
		for id, cid := range contentIDs(p) {
			if cid != "" {
				t.Errorf("%s: title %d marked without a viewer", tc.name, id)
			}
		}
	}
	// ListLibraries with an empty user lists every library, so it must
	// never be called without one.
	for _, u := range host.userIDs {
		if u == "" {
			t.Fatal("ListLibraries was called without a user id")
		}
	}
}

func TestPresenceBatches(t *testing.T) {
	host := &fakeHost{libraries: map[string][]string{"7": {"1"}}}
	a := testApp(t, host)
	var credits []Credit
	for i := 1; i <= 250; i++ {
		credits = append(credits, Credit{ID: i, Media: "movie"}, Credit{ID: i, Media: "movie"})
	}
	if _, err := a.library(context.Background(), "1", credits, "7"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(host.batches) != "[100 100 50]" {
		t.Errorf("batches = %v, want [100 100 50] (duplicates merged)", host.batches)
	}
}

func TestRequestSetting(t *testing.T) {
	a := testApp(t, defaultHost())
	if p, _ := a.person(context.Background(), "31", "7"); !p.CanRequest {
		t.Error("Request button should be on by default")
	}
	a.configure(entries(map[string]map[string]any{
		"tmdb":     {"api_key": "k"},
		"requests": {"hide_request_button": true},
	}))
	if p, _ := a.person(context.Background(), "31", "7"); p.CanRequest {
		t.Error("Request button should follow the admin setting")
	}
}

func TestPageCSP(t *testing.T) {
	html := string(page)
	if strings.Contains(html, "__CSP__") {
		t.Fatal("CSP placeholder was not filled in")
	}
	start := strings.Index(html, "<script>") + len("<script>")
	end := strings.Index(html, "</script>")
	sum := sha256.Sum256([]byte(html[start:end]))
	want := "script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(html, want) {
		t.Errorf("CSP does not allow the page script by hash")
	}
	if strings.Index(html, `http-equiv="Content-Security-Policy"`) > strings.Index(html, "<script>") {
		t.Error("CSP must come before the script")
	}
}

func TestRoutes(t *testing.T) {
	a := testApp(t, defaultHost())
	s := &routeServer{app: a}
	call := func(method, path string, q map[string]any, headers map[string]string) *pluginv1.HandleHTTPResponse {
		st, _ := structpb.NewStruct(q)
		r, err := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: method, Path: path, Query: st, Headers: headers})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	get := func(path string, q map[string]any) *pluginv1.HandleHTTPResponse { return call("GET", path, q, nil) }
	if r := get("/filmography/", nil); r.StatusCode != 200 || !strings.Contains(string(r.Body), "<title>Filmography</title>") {
		t.Errorf("page = %d", r.StatusCode)
	}
	r := get("/api/search", map[string]any{"q": []any{"tom"}})
	if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"Tom Hanks"`) || strings.Contains(string(r.Body), `"X"`) {
		t.Errorf("search = %d %s", r.StatusCode, r.Body)
	}
	// Silo's identity header reaches library matching, whatever its case.
	r = call("GET", "/api/person/31", nil, map[string]string{"x-silo-user-id": "7"})
	if r.StatusCode != 200 || !strings.Contains(string(r.Body), `"movie-tmdb-862"`) || !strings.Contains(string(r.Body), `"can_request":true`) {
		t.Errorf("person = %d %s", r.StatusCode, r.Body)
	}
	if r := get("/api/person/31", nil); r.StatusCode != 200 || strings.Contains(string(r.Body), `"movie-tmdb-862"`) {
		t.Errorf("person without a viewer = %d %s", r.StatusCode, r.Body)
	}
	if r := get("/api/person/../x", nil); r.StatusCode != 400 {
		t.Errorf("bad id = %d", r.StatusCode)
	}
	if r := get("/api/person/4040", nil); r.StatusCode != 404 {
		t.Errorf("unknown person = %d %s", r.StatusCode, r.Body)
	}
	if r := get("/api/silo-person/77", nil); r.StatusCode != 404 {
		t.Errorf("removed silo-person route = %d", r.StatusCode)
	}
	if r := call("POST", "/api/person/31", nil, nil); r.StatusCode != 405 {
		t.Errorf("POST = %d", r.StatusCode)
	}
	a.configure(entries(map[string]map[string]any{"tmdb": {"api_key": "wrong"}}))
	if r := get("/api/person/31", nil); r.StatusCode != 502 || !strings.Contains(string(r.Body), "Invalid API key") {
		t.Errorf("bad key = %d %s", r.StatusCode, r.Body)
	}
}

type failingHost struct{ fakeHost }

func (failingHost) ListLibraries(context.Context, string) ([]*pluginv1.Library, error) {
	return nil, errors.New("rpc error: internal detail 10.0.0.5:5432")
}

func TestHostErrorsStayInTheLog(t *testing.T) {
	a := testApp(t, failingHost{})
	p, err := a.person(context.Background(), "31", "7")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.LibraryError, "10.0.0.5") || !strings.Contains(p.LibraryError, "couldn't check the library") {
		t.Errorf("library error = %q", p.LibraryError)
	}
}
