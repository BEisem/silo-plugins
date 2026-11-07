package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// Config is the admin form configuration Silo delivers through Configure.
type Config struct {
	TMDBKey   string
	Language  string
	APIKey    string
	ProfileID string
	BaseURL   string
}

func parseConfig(entries []*pluginv1.ConfigEntry) Config {
	var cfg Config
	for _, e := range entries {
		if e == nil || e.GetValue() == nil {
			continue
		}
		v := e.GetValue().AsMap()
		get := func(k string) string { s, _ := v[k].(string); return strings.TrimSpace(s) }
		switch e.GetKey() {
		case "tmdb":
			cfg.TMDBKey = get("api_key")
			cfg.Language = get("language")
		case "silo":
			cfg.APIKey = get("api_key")
			cfg.ProfileID = get("profile_id")
			cfg.BaseURL = strings.TrimRight(get("base_url"), "/")
		}
	}
	return cfg
}

type App struct {
	log hclog.Logger

	mu  sync.RWMutex
	cfg Config

	profileMu sync.Mutex
	profileID string

	cache cache

	// tmdbOf maps a Silo series content id to its TMDB id. Ids don't
	// change, so it is never expired.
	idMu   sync.Mutex
	tmdbOf map[string]string

	// tmdbBase replaces the TMDB API address in tests.
	tmdbBase string
}

const (
	personTTL  = 6 * time.Hour
	searchTTL  = 30 * time.Minute
	libraryTTL = 5 * time.Minute
)

func newApp(log hclog.Logger) *App {
	return &App{log: log, tmdbOf: map[string]string{}}
}

func (a *App) configure(entries []*pluginv1.ConfigEntry) {
	a.mu.Lock()
	a.cfg = parseConfig(entries)
	a.mu.Unlock()
	a.cache.clear()
}

func (a *App) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *App) tmdb() (*TMDB, error) {
	cfg := a.config()
	if cfg.TMDBKey == "" {
		return nil, errors.New("no TMDB API key: an admin needs to add one in this plugin's TMDB settings under Admin → Plugins")
	}
	t := newTMDB(cfg.TMDBKey, cfg.Language)
	if a.tmdbBase != "" {
		t.base = a.tmdbBase
	}
	return t, nil
}

// wikiLang is the Wikipedia edition links point at.
func (a *App) wikiLang() string {
	lang := strings.ToLower(a.config().Language)
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	if lang == "" {
		return "en"
	}
	return lang
}

func (a *App) endpoint(ctx context.Context) string {
	if base := a.config().BaseURL; base != "" {
		return base
	}
	if h := sdkruntime.Host(); h != nil {
		if info, err := h.GetHostInfo(ctx); err == nil && info.InternalBaseURL != "" {
			return loopbackURL(info.InternalBaseURL)
		}
	}
	return "http://127.0.0.1:8080"
}

func loopbackURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return strings.TrimRight(raw, "/")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	}
	return strings.TrimRight(u.String(), "/")
}

func (a *App) client(ctx context.Context) (*SiloClient, error) {
	cfg := a.config()
	if cfg.APIKey == "" {
		return nil, errors.New("no Silo API key, so library titles aren't marked: an admin can add one in this plugin's Silo connection settings")
	}
	c := newSiloClient(a.endpoint(ctx), cfg.APIKey, cfg.ProfileID)
	if cfg.ProfileID != "" {
		return c, nil
	}
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	if a.profileID == "" {
		id, err := c.PrimaryProfile(ctx)
		if err != nil {
			return nil, fmt.Errorf("finding a profile for the API key: %w", err)
		}
		a.profileID = id
	}
	c.profile = a.profileID
	return c, nil
}

// person returns a person's page data with library titles marked.
func (a *App) person(ctx context.Context, id string) (*Person, error) {
	var base *Person
	if v, ok := a.cache.get("person:" + id); ok {
		base = v.(*Person)
	} else {
		t, err := a.tmdb()
		if err != nil {
			return nil, err
		}
		raw, err := t.Person(ctx, id)
		if err != nil {
			return nil, err
		}
		base = buildPerson(raw, a.wikiLang())
		a.cache.put("person:"+id, base, personTTL)
	}

	// Copy so marking the library never touches the cached value.
	p := *base
	p.Credits = append([]Credit(nil), base.Credits...)
	lib, err := a.library(ctx, p.Name, id)
	if err != nil {
		a.log.Warn("library lookup failed", "person", id, "error", err)
		p.LibraryError = err.Error()
		return &p, nil
	}
	p.SiloPersonID = lib.personID
	for i := range p.Credits {
		c := &p.Credits[i]
		c.ContentID = lib.items[fmt.Sprintf("%s:%d", c.Media, c.ID)]
	}
	return &p, nil
}

type libraryMatch struct {
	personID string
	// items maps "movie:<tmdb id>" and "tv:<tmdb id>" to Silo content ids.
	items map[string]string
}

// library finds the titles Silo credits the person on.
func (a *App) library(ctx context.Context, name, tmdbID string) (*libraryMatch, error) {
	if v, ok := a.cache.get("library:" + tmdbID); ok {
		return v.(*libraryMatch), nil
	}
	c, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	m := &libraryMatch{items: map[string]string{}}
	sp, err := c.findPerson(ctx, name, tmdbID)
	if err != nil {
		return nil, err
	}
	if sp != nil {
		m.personID = sp.ID
		cards, err := c.personTitles(ctx, sp.ID)
		if err != nil {
			return nil, err
		}
		for _, card := range cards {
			media, tid := a.cardTMDB(ctx, c, card)
			if tid != "" {
				m.items[media+":"+tid] = card.ContentID
			}
		}
	}
	a.cache.put("library:"+tmdbID, m, libraryTTL)
	return m, nil
}

// cardTMDB works out a library item's TMDB id. Movie and some series ids
// carry it ("movie-tmdb-862"); other series are looked up once.
func (a *App) cardTMDB(ctx context.Context, c *SiloClient, card libraryCard) (media, id string) {
	switch card.Type {
	case "movie":
		media = "movie"
	case "series":
		media = "tv"
	default:
		return "", ""
	}
	if rest, ok := strings.CutPrefix(card.ContentID, card.Type+"-tmdb-"); ok {
		return media, rest
	}
	a.idMu.Lock()
	id, known := a.tmdbOf[card.ContentID]
	a.idMu.Unlock()
	if known {
		return media, id
	}
	id, err := c.itemTMDBID(ctx, card.ContentID)
	if err != nil {
		a.log.Warn("item lookup failed", "content_id", card.ContentID, "error", err)
		return media, ""
	}
	a.idMu.Lock()
	a.tmdbOf[card.ContentID] = id
	a.idMu.Unlock()
	return media, id
}

func (a *App) search(ctx context.Context, q string) ([]PersonSummary, error) {
	key := "search:" + strings.ToLower(q)
	if v, ok := a.cache.get(key); ok {
		return v.([]PersonSummary), nil
	}
	t, err := a.tmdb()
	if err != nil {
		return nil, err
	}
	var res []PersonSummary
	if q == "" {
		res, err = t.Popular(ctx)
	} else {
		res, err = t.Search(ctx, q)
	}
	if err != nil {
		return nil, err
	}
	a.cache.put(key, res, searchTTL)
	return res, nil
}

// siloPersonTMDB resolves a Silo person id to their TMDB id.
func (a *App) siloPersonTMDB(ctx context.Context, id string) (string, error) {
	c, err := a.client(ctx)
	if err != nil {
		return "", err
	}
	p, err := c.person(ctx, id)
	if err != nil {
		return "", err
	}
	if p.TMDBID == "" {
		return "", fmt.Errorf("%s has no TMDB id in Silo", p.Name)
	}
	return p.TMDBID, nil
}
