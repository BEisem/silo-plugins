package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

// Config is the admin form configuration Silo delivers through Configure.
type Config struct {
	TMDBKey  string
	Language string
	// ShowRequests offers a Request button on titles that aren't in the
	// library. It is on unless an admin turns it off.
	ShowRequests bool
}

func parseConfig(entries []*pluginv1.ConfigEntry) Config {
	cfg := Config{ShowRequests: true}
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
		case "requests":
			if hide, ok := v["hide_request_button"].(bool); ok {
				cfg.ShowRequests = !hide
			}
		}
	}
	return cfg
}

// libraryHost is the part of Silo's plugin host the plugin uses to mark
// library titles. Both calls go over the plugin's own connection to Silo,
// so the plugin needs no Silo API key.
type libraryHost interface {
	ListLibraries(ctx context.Context, userID string) ([]*pluginv1.Library, error)
	CheckMediaPresence(ctx context.Context, provider, mediaType string, ids []string) (map[string]runtimehost.MediaPresence, error)
}

type App struct {
	log hclog.Logger

	mu  sync.RWMutex
	cfg Config

	cache cache

	// host returns Silo's plugin host, or nil before it is connected.
	// Tests replace it.
	host func() libraryHost

	// tmdbBase replaces the TMDB API address in tests.
	tmdbBase string
}

const (
	personTTL  = 6 * time.Hour
	searchTTL  = 30 * time.Minute
	libraryTTL = 5 * time.Minute

	// presenceBatch is the most ids Silo checks in one CheckMediaPresence call.
	presenceBatch = 100
)

func newApp(log hclog.Logger) *App {
	return &App{log: log, host: siloHost}
}

// siloHost returns the connected host. It returns a nil interface, not an
// interface holding a nil pointer, while the host isn't connected.
func siloHost() libraryHost {
	if h := sdkruntime.Host(); h != nil {
		return h
	}
	return nil
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

// siloUserID matches the user id Silo stamps on plugin requests.
var siloUserID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// person returns a person's page data with the titles the viewer can see in
// the library marked. userID is the viewer's Silo user id, as Silo stamped
// it on the request.
func (a *App) person(ctx context.Context, id, userID string) (*Person, error) {
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
	p.CanRequest = a.config().ShowRequests
	items, err := a.library(ctx, id, p.Credits, userID)
	if err != nil {
		a.log.Warn("library lookup failed", "person", id, "error", err)
		// Host errors stay in the log; the viewer gets a plain sentence.
		var shown viewerError
		if errors.As(err, &shown) {
			p.LibraryError = shown.Error()
		} else {
			p.LibraryError = "couldn't check the library right now, so library titles aren't marked"
		}
		return &p, nil
	}
	for i := range p.Credits {
		c := &p.Credits[i]
		c.ContentID = items[creditKey(c.Media, c.ID)]
	}
	return &p, nil
}

func creditKey(media string, id int) string { return fmt.Sprintf("%s:%d", media, id) }

// viewerError is an error whose text is fit to show on the page.
type viewerError string

func (e viewerError) Error() string { return string(e) }

// library maps "movie:<tmdb id>" and "tv:<tmdb id>" to Silo content ids for
// the credits that are in a library the viewer may see.
func (a *App) library(ctx context.Context, personID string, credits []Credit, userID string) (map[string]string, error) {
	if !siloUserID.MatchString(userID) {
		// Silo stamps the viewer on every signed-in request. Without it the
		// plugin can't tell which libraries to show, so it shows none.
		return nil, viewerError("couldn't tell who is signed in, so library titles aren't marked")
	}
	h := a.host()
	if h == nil {
		return nil, viewerError("not connected to Silo yet, so library titles aren't marked; reload in a moment")
	}
	visible, err := a.visibleLibraries(ctx, h, userID)
	if err != nil {
		return nil, err
	}
	present, err := a.presence(ctx, h, personID, credits)
	if err != nil {
		return nil, err
	}
	items := make(map[string]string, len(present))
	for key, m := range present {
		if visible[m.LibraryID] {
			items[key] = m.MediaID
		}
	}
	return items, nil
}

// visibleLibraries returns the ids of the libraries Silo lets the user see.
func (a *App) visibleLibraries(ctx context.Context, h libraryHost, userID string) (map[string]bool, error) {
	key := "libraries:" + userID
	if v, ok := a.cache.get(key); ok {
		return v.(map[string]bool), nil
	}
	libs, err := h.ListLibraries(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("listing your libraries: %w", err)
	}
	visible := make(map[string]bool, len(libs))
	for _, l := range libs {
		if id := l.GetId(); id != "" {
			visible[id] = true
		}
	}
	a.cache.put(key, visible, libraryTTL)
	return visible, nil
}

// presence asks Silo which of a person's titles it has, whoever may see
// them. The answer is the same for every viewer, so it is cached per person
// and filtered per viewer afterwards.
func (a *App) presence(ctx context.Context, h libraryHost, personID string, credits []Credit) (map[string]runtimehost.MediaPresence, error) {
	key := "presence:" + personID
	if v, ok := a.cache.get(key); ok {
		return v.(map[string]runtimehost.MediaPresence), nil
	}
	ids := map[string][]string{}
	seen := map[string]bool{}
	for _, c := range credits {
		if c.Media != "movie" && c.Media != "tv" {
			continue
		}
		k := creditKey(c.Media, c.ID)
		if !seen[k] {
			seen[k] = true
			ids[c.Media] = append(ids[c.Media], fmt.Sprint(c.ID))
		}
	}
	out := map[string]runtimehost.MediaPresence{}
	for _, media := range []string{"movie", "tv"} {
		list := ids[media]
		for start := 0; start < len(list); start += presenceBatch {
			batch := list[start:min(start+presenceBatch, len(list))]
			found, err := h.CheckMediaPresence(ctx, "tmdb", media, batch)
			if err != nil {
				return nil, fmt.Errorf("checking the library: %w", err)
			}
			for id, m := range found {
				out[media+":"+id] = m
			}
		}
	}
	a.cache.put(key, out, libraryTTL)
	return out, nil
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
