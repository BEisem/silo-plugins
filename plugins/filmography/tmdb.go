package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tmdbBase = "https://api.themoviedb.org/3"

type TMDB struct {
	key      string
	language string
	base     string
	http     *http.Client
}

func newTMDB(key, language string) *TMDB {
	return &TMDB{key: key, language: language, base: tmdbBase, http: &http.Client{Timeout: 20 * time.Second}}
}

// get calls TMDB. A v4 read access token (a JWT) goes in the Authorization
// header; a v3 key goes in the query string.
func (t *TMDB) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	bearer := strings.Count(t.key, ".") == 2
	if !bearer {
		q.Set("api_key", t.key)
	}
	if t.language != "" {
		q.Set("language", t.language)
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+t.key)
		}
		resp, err := t.http.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = min(time.Duration(s)*time.Second, 10*time.Second)
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode >= 300 {
			var problem struct {
				Message string `json:"status_message"`
			}
			_ = json.Unmarshal(body, &problem)
			return &apiError{Status: resp.StatusCode, Detail: "TMDB: " + problem.Message}
		}
		return json.Unmarshal(body, out)
	}
}

// PersonSummary is a search or popular-people result.
type PersonSummary struct {
	ID         int      `json:"id"`
	Name       string   `json:"name"`
	Photo      string   `json:"photo,omitempty"`
	Department string   `json:"department,omitempty"`
	KnownFor   []string `json:"known_for,omitempty"`
}

type tmdbPersonResult struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ProfilePath string `json:"profile_path"`
	Department  string `json:"known_for_department"`
	Adult       bool   `json:"adult"`
	KnownFor    []struct {
		Title string `json:"title"`
		Name  string `json:"name"`
	} `json:"known_for"`
}

func (t *TMDB) people(ctx context.Context, path string, q url.Values) ([]PersonSummary, error) {
	var resp struct {
		Results []tmdbPersonResult `json:"results"`
	}
	if err := t.get(ctx, path, q, &resp); err != nil {
		return nil, err
	}
	out := make([]PersonSummary, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Adult {
			continue
		}
		s := PersonSummary{ID: r.ID, Name: r.Name, Photo: r.ProfilePath, Department: r.Department}
		for _, k := range r.KnownFor {
			if title := cmp(k.Title, k.Name); title != "" {
				s.KnownFor = append(s.KnownFor, title)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func (t *TMDB) Search(ctx context.Context, query string) ([]PersonSummary, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("include_adult", "false")
	return t.people(ctx, "/search/person", q)
}

func (t *TMDB) Popular(ctx context.Context) ([]PersonSummary, error) {
	return t.people(ctx, "/person/popular", nil)
}

type tmdbCredit struct {
	ID           int     `json:"id"`
	MediaType    string  `json:"media_type"`
	Title        string  `json:"title"`
	Name         string  `json:"name"`
	ReleaseDate  string  `json:"release_date"`
	FirstAirDate string  `json:"first_air_date"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Overview     string  `json:"overview"`
	Popularity   float64 `json:"popularity"`
	VoteAverage  float64 `json:"vote_average"`
	VoteCount    int     `json:"vote_count"`
	GenreIDs     []int   `json:"genre_ids"`
	Adult        bool    `json:"adult"`
	Character    string  `json:"character"`
	Job          string  `json:"job"`
	Department   string  `json:"department"`
	EpisodeCount int     `json:"episode_count"`
	Order        *int    `json:"order"`
}

type tmdbPerson struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	AlsoKnownAs  []string `json:"also_known_as"`
	Biography    string   `json:"biography"`
	Birthday     string   `json:"birthday"`
	Deathday     string   `json:"deathday"`
	PlaceOfBirth string   `json:"place_of_birth"`
	Department   string   `json:"known_for_department"`
	Homepage     string   `json:"homepage"`
	ProfilePath  string   `json:"profile_path"`
	IMDbID       string   `json:"imdb_id"`
	Credits      struct {
		Cast []tmdbCredit `json:"cast"`
		Crew []tmdbCredit `json:"crew"`
	} `json:"combined_credits"`
	Images struct {
		Profiles []struct {
			FilePath string  `json:"file_path"`
			Width    int     `json:"width"`
			Height   int     `json:"height"`
			Votes    float64 `json:"vote_average"`
		} `json:"profiles"`
	} `json:"images"`
	ExternalIDs struct {
		IMDb      string `json:"imdb_id"`
		Wikidata  string `json:"wikidata_id"`
		Facebook  string `json:"facebook_id"`
		Instagram string `json:"instagram_id"`
		TikTok    string `json:"tiktok_id"`
		Twitter   string `json:"twitter_id"`
		YouTube   string `json:"youtube_id"`
	} `json:"external_ids"`
}

func (t *TMDB) Person(ctx context.Context, id string) (*tmdbPerson, error) {
	q := url.Values{}
	q.Set("append_to_response", "combined_credits,images,external_ids")
	var p tmdbPerson
	if err := t.get(ctx, "/person/"+url.PathEscape(id), q, &p); err != nil {
		return nil, err
	}
	// Biographies are often missing in other languages; fall back to English.
	if p.Biography == "" && t.language != "" && !strings.HasPrefix(t.language, "en") {
		var en struct {
			Biography string `json:"biography"`
		}
		eng := *t
		eng.language = "en-US"
		if eng.get(ctx, "/person/"+url.PathEscape(id), nil, &en) == nil {
			p.Biography = en.Biography
		}
	}
	return &p, nil
}

// Person is the page's view of one person.
type Person struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	AlsoKnownAs []string `json:"also_known_as,omitempty"`
	Biography   string   `json:"biography,omitempty"`
	Birthday    string   `json:"birthday,omitempty"`
	Deathday    string   `json:"deathday,omitempty"`
	Birthplace  string   `json:"birthplace,omitempty"`
	Department  string   `json:"department,omitempty"`
	Photo       string   `json:"photo,omitempty"`
	Images      []Image  `json:"images"`
	Links       []Link   `json:"links"`
	Credits     []Credit `json:"credits"`
	// SiloPersonID is the person's id in Silo, when the library has them.
	SiloPersonID string `json:"silo_person_id,omitempty"`
	// LibraryError explains why library titles couldn't be marked.
	LibraryError string `json:"library_error,omitempty"`
}

type Image struct {
	Path   string `json:"path"`
	Width  int    `json:"w"`
	Height int    `json:"h"`
}

type Link struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Role struct {
	Crew     bool   `json:"crew,omitempty"`
	Text     string `json:"text,omitempty"`
	Episodes int    `json:"episodes,omitempty"`
}

// Credit is one title, with every role the person had on it.
type Credit struct {
	ID         int     `json:"id"`
	Media      string  `json:"media"` // movie or tv
	Title      string  `json:"title"`
	Date       string  `json:"date,omitempty"`
	Poster     string  `json:"poster,omitempty"`
	Backdrop   string  `json:"backdrop,omitempty"`
	Overview   string  `json:"overview,omitempty"`
	Popularity float64 `json:"popularity"`
	Vote       float64 `json:"vote"`
	Votes      int     `json:"votes"`
	Acting     bool    `json:"acting"`
	Crew       bool    `json:"crew"`
	// Self marks acting credits that are only appearances as themselves:
	// talk shows, news, award shows and documentaries where the character
	// is "Self". Crew roles on the same title are unaffected.
	Self    bool   `json:"self,omitempty"`
	Billing int    `json:"billing"`
	Roles   []Role `json:"roles"`
	// ContentID is the Silo item, when the title is in the library.
	ContentID string `json:"content_id,omitempty"`
}

// Talk, news and reality genres hold guest appearances, not roles.
var appearanceGenres = map[int]bool{10767: true, 10763: true, 10764: true}

func isSelf(c tmdbCredit) bool {
	ch := strings.ToLower(strings.TrimSpace(c.Character))
	for _, s := range []string{"self", "himself", "herself", "themselves", "themself"} {
		if ch == s || strings.HasPrefix(ch, s+" ") || strings.HasPrefix(ch, s+" (") || strings.HasPrefix(ch, s+" -") {
			return true
		}
	}
	if strings.HasSuffix(ch, "- self") || strings.Contains(ch, "(archive footage)") {
		return true
	}
	for _, g := range c.GenreIDs {
		if appearanceGenres[g] {
			return true
		}
	}
	return false
}

func buildPerson(p *tmdbPerson, wikiLang string) *Person {
	out := &Person{
		ID:          p.ID,
		Name:        p.Name,
		AlsoKnownAs: p.AlsoKnownAs,
		Biography:   p.Biography,
		Birthday:    p.Birthday,
		Deathday:    p.Deathday,
		Birthplace:  p.PlaceOfBirth,
		Department:  p.Department,
		Photo:       p.ProfilePath,
		Images:      []Image{},
		Links:       personLinks(p, wikiLang),
	}
	for _, im := range p.Images.Profiles {
		out.Images = append(out.Images, Image{Path: im.FilePath, Width: im.Width, Height: im.Height})
	}
	// Show the current headshot first.
	sort.SliceStable(out.Images, func(i, j int) bool { return out.Images[i].Path == p.ProfilePath && out.Images[j].Path != p.ProfilePath })

	byKey := map[string]*Credit{}
	var order []string
	add := func(c tmdbCredit, crew bool) {
		if c.Adult || (c.MediaType != "movie" && c.MediaType != "tv") {
			return
		}
		key := fmt.Sprintf("%s:%d", c.MediaType, c.ID)
		cr := byKey[key]
		if cr == nil {
			cr = &Credit{
				ID:         c.ID,
				Media:      c.MediaType,
				Title:      cmp(c.Title, c.Name),
				Date:       cmp(c.ReleaseDate, c.FirstAirDate),
				Poster:     c.PosterPath,
				Backdrop:   c.BackdropPath,
				Overview:   c.Overview,
				Popularity: c.Popularity,
				Vote:       c.VoteAverage,
				Votes:      c.VoteCount,
				Billing:    999,
				Roles:      []Role{},
			}
			byKey[key] = cr
			order = append(order, key)
		}
		if crew {
			cr.Crew = true
			for _, r := range cr.Roles {
				if r.Crew && r.Text == c.Job {
					return
				}
			}
			cr.Roles = append(cr.Roles, Role{Crew: true, Text: c.Job, Episodes: c.EpisodeCount})
			return
		}
		// Self holds while every acting role is an appearance as themselves.
		cr.Self = isSelf(c) && (cr.Self || !cr.Acting)
		cr.Acting = true
		if c.Order != nil && *c.Order < cr.Billing {
			cr.Billing = *c.Order
		}
		cr.Roles = append(cr.Roles, Role{Text: c.Character, Episodes: c.EpisodeCount})
	}
	for _, c := range p.Credits.Cast {
		add(c, false)
	}
	for _, c := range p.Credits.Crew {
		add(c, true)
	}
	out.Credits = make([]Credit, 0, len(order))
	for _, k := range order {
		out.Credits = append(out.Credits, *byKey[k])
	}
	return out
}

func personLinks(p *tmdbPerson, wikiLang string) []Link {
	links := []Link{{Kind: "tmdb", Label: "TMDB", URL: fmt.Sprintf("https://www.themoviedb.org/person/%d", p.ID)}}
	x := p.ExternalIDs
	imdb := cmp(x.IMDb, p.IMDbID)
	if imdb != "" {
		links = append(links, Link{Kind: "imdb", Label: "IMDb", URL: "https://www.imdb.com/name/" + url.PathEscape(imdb) + "/"})
	}
	if x.Wikidata != "" {
		// Wikidata redirects to the person's article in the chosen language.
		links = append(links,
			Link{Kind: "wikipedia", Label: "Wikipedia", URL: "https://www.wikidata.org/wiki/Special:GoToLinkedPage/" + wikiLang + "wiki/" + url.PathEscape(x.Wikidata)},
			Link{Kind: "wikidata", Label: "Wikidata", URL: "https://www.wikidata.org/wiki/" + url.PathEscape(x.Wikidata)})
	} else {
		links = append(links, Link{Kind: "wikipedia", Label: "Wikipedia", URL: "https://" + wikiLang + ".wikipedia.org/w/index.php?search=" + url.QueryEscape(p.Name)})
	}
	if x.Instagram != "" {
		links = append(links, Link{Kind: "instagram", Label: "Instagram", URL: "https://www.instagram.com/" + url.PathEscape(x.Instagram) + "/"})
	}
	if x.Twitter != "" {
		links = append(links, Link{Kind: "x", Label: "X", URL: "https://x.com/" + url.PathEscape(x.Twitter)})
	}
	if x.TikTok != "" {
		links = append(links, Link{Kind: "tiktok", Label: "TikTok", URL: "https://www.tiktok.com/@" + url.PathEscape(x.TikTok)})
	}
	if x.Facebook != "" {
		links = append(links, Link{Kind: "facebook", Label: "Facebook", URL: "https://www.facebook.com/" + url.PathEscape(x.Facebook)})
	}
	if x.YouTube != "" {
		links = append(links, Link{Kind: "youtube", Label: "YouTube", URL: "https://www.youtube.com/" + url.PathEscape(x.YouTube)})
	}
	if h := strings.TrimSpace(p.Homepage); strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		links = append(links, Link{Kind: "homepage", Label: "Homepage", URL: h})
	}
	return links
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cache keeps TMDB answers for a while; it is cleared when it grows large.
type cache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	v   any
	exp time.Time
}

func (c *cache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Now().After(e.exp) {
		return nil, false
	}
	return e.v, true
}

func (c *cache) put(key string, v any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 500 {
		c.m = map[string]cacheEntry{}
	}
	c.m[key] = cacheEntry{v: v, exp: time.Now().Add(ttl)}
}

func (c *cache) clear() {
	c.mu.Lock()
	c.m = nil
	c.mu.Unlock()
}
