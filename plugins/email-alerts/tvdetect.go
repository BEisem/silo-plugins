package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// New episodes are detected from availability, not from the catalog's
// added_at: Silo creates episode items from series metadata long before
// their files arrive, so a newly downloaded episode keeps the old date.
//
// The "Recently Added TV" row of each TV library is ordered by arrival, so
// its first shows are the ones that may have new episodes. For each, the
// per-season count of available episodes is compared with the last check;
// a season whose count went up is listed, and its episodes whose files are
// newer than any file seen before are arrivals. Quality upgrades replace a
// file without changing the count, so they don't notify.

const (
	keyTVState  = "tv-state"
	keyArrivals = "arrivals"
	// tvCandidates is how many shows from the front of each row are checked.
	tvCandidates = 12
	arrivalsCap  = 1500
	arrivalsKeep = 9 * 24 * time.Hour
)

type tvState struct {
	Initialized bool `json:"init"`
	// FileWatermark is the highest media file id seen on an episode. File ids
	// increase as files are added, so a higher id is a newer file.
	FileWatermark int64 `json:"fw"`
	// Seasons maps series id to season number to available episode count.
	Seasons map[string]map[string]int `json:"s"`
}

type arrival struct {
	ID      string `json:"i"`
	Series  string `json:"s"`
	Library string `json:"l"`
	At      int64  `json:"t"`
}

type seasonRow struct {
	SeasonNumber int `json:"season_number"`
	EpisodeCount int `json:"episode_count"`
}

type episodeRow struct {
	ContentID string `json:"content_id"`
	Files     []struct {
		FileID string `json:"file_id"`
	} `json:"files"`
}

func (e episodeRow) newestFile() int64 {
	var max int64
	for _, f := range e.Files {
		if id, err := strconv.ParseInt(f.FileID, 10, 64); err == nil && id > max {
			max = id
		}
	}
	return max
}

// recentlyAddedSection finds the id of a library's Recently Added row.
func (c *SiloClient) recentlyAddedSection(ctx context.Context, libraryID string) (string, error) {
	var resp struct {
		Sections []struct {
			ID          string `json:"id"`
			SectionType string `json:"section_type"`
		} `json:"sections"`
	}
	if err := c.get(ctx, "/api/v2/library/"+url.PathEscape(libraryID)+"/sections", nil, &resp); err != nil {
		return "", err
	}
	for _, s := range resp.Sections {
		if s.SectionType == "recently_added" {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("library %s has no Recently Added row", libraryID)
}

func (c *SiloClient) sectionSeries(ctx context.Context, libraryID, sectionID string) ([]Card, error) {
	var resp struct {
		Items []Card `json:"items"`
	}
	err := c.get(ctx, "/api/v2/library/"+url.PathEscape(libraryID)+"/sections/"+url.PathEscape(sectionID)+"/items", nil, &resp)
	return resp.Items, err
}

func (c *SiloClient) seasons(ctx context.Context, seriesID string) ([]seasonRow, error) {
	var resp struct {
		Items []seasonRow `json:"items"`
	}
	err := c.get(ctx, "/api/v2/catalog/series/"+url.PathEscape(seriesID)+"/seasons", nil, &resp)
	return resp.Items, err
}

func (c *SiloClient) seasonEpisodes(ctx context.Context, seriesID string, season int) ([]episodeRow, error) {
	var resp struct {
		Items []episodeRow `json:"items"`
	}
	err := c.get(ctx, "/api/v2/catalog/series/"+url.PathEscape(seriesID)+"/seasons/"+strconv.Itoa(season)+"/episodes", nil, &resp)
	return resp.Items, err
}

// rowSeries returns up to limit distinct shows from the front of a Recently
// Added row. A show with one new episode appears as that episode's card, so
// episode cards stand for their series.
func rowSeries(cards []Card, limit int) []Card {
	var out []Card
	seen := map[string]bool{}
	for _, c := range cards {
		if c.Type == "episode" {
			if c.SeriesID == "" {
				continue
			}
			c = Card{ContentID: c.SeriesID, Type: "series", Title: c.SeriesTitle}
		}
		if c.ContentID == "" || seen[c.ContentID] {
			continue
		}
		seen[c.ContentID] = true
		out = append(out, c)
		if len(out) == limit {
			break
		}
	}
	return out
}

// tvLibraries returns the libraries that hold episodes.
func (a *App) tvLibraries(ctx context.Context) []LibraryRef {
	var out []LibraryRef
	for _, l := range a.libraries(ctx) {
		if l.GetId() == "" {
			continue
		}
		for _, t := range libraryTypes(l.GetMediaType()) {
			if t == "episode" {
				out = append(out, LibraryRef{ID: l.GetId(), Name: l.GetName()})
			}
		}
	}
	return out
}

// detectTV records episodes that became available since the last check. The
// first run only takes a baseline, so the existing library isn't announced.
func (a *App) detectTV(ctx context.Context, client *SiloClient) (int, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	var st tvState
	if _, err := a.store.read(ctx, keyTVState, &st); err != nil {
		return 0, err
	}
	if st.Seasons == nil {
		st.Seasons = map[string]map[string]int{}
	}
	now := time.Now()
	var (
		found   []arrival
		maxSeen = st.FileWatermark
		keep    = map[string]bool{}
		errs    []error
	)
	for _, lib := range a.tvLibraries(ctx) {
		sid, err := client.recentlyAddedSection(ctx, lib.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		series, err := client.sectionSeries(ctx, lib.ID, sid)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, s := range rowSeries(series, tvCandidates) {
			keep[s.ContentID] = true
			rows, err := client.seasons(ctx, s.ContentID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			counts := map[string]int{}
			for _, r := range rows {
				counts[strconv.Itoa(r.SeasonNumber)] = r.EpisodeCount
			}
			old, known := st.Seasons[s.ContentID]
			for _, r := range rows {
				key := strconv.Itoa(r.SeasonNumber)
				if r.EpisodeCount == 0 || (known && r.EpisodeCount <= old[key]) {
					continue
				}
				eps, err := client.seasonEpisodes(ctx, s.ContentID, r.SeasonNumber)
				if err != nil {
					errs = append(errs, err)
					// Leave the old count so the season is checked again.
					if known {
						counts[key] = old[key]
					} else {
						delete(counts, key)
					}
					continue
				}
				for _, e := range eps {
					newest := e.newestFile()
					if newest > maxSeen {
						maxSeen = newest
					}
					if st.Initialized && newest > st.FileWatermark {
						found = append(found, arrival{ID: e.ContentID, Series: s.ContentID, Library: lib.ID, At: now.UnixMilli()})
					}
				}
			}
			st.Seasons[s.ContentID] = counts
		}
	}
	// Forget shows that dropped out of the rows; if one comes back, its
	// seasons are listed again and the file watermark decides what's new.
	for id := range st.Seasons {
		if !keep[id] {
			delete(st.Seasons, id)
		}
	}
	st.FileWatermark = maxSeen
	st.Initialized = true
	if err := a.store.write(ctx, keyTVState, st); err != nil {
		return 0, err
	}
	if len(found) > 0 {
		if err := a.appendArrivals(ctx, found); err != nil {
			return 0, err
		}
		a.log.Info("new episodes detected", "count", len(found))
	}
	if len(errs) > 0 {
		return len(found), fmt.Errorf("checking TV libraries: %v", errs[0])
	}
	return len(found), nil
}

func (a *App) arrivals(ctx context.Context) ([]arrival, error) {
	var list []arrival
	_, err := a.store.read(ctx, keyArrivals, &list)
	return list, err
}

// appendArrivals must be called with the store lock held.
func (a *App) appendArrivals(ctx context.Context, add []arrival) error {
	list, err := a.arrivals(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, x := range list {
		seen[x.ID] = true
	}
	for _, x := range add {
		if !seen[x.ID] {
			list = append(list, x)
			seen[x.ID] = true
		}
	}
	cutoff := time.Now().Add(-arrivalsKeep).UnixMilli()
	kept := list[:0]
	for _, x := range list {
		if x.At >= cutoff {
			kept = append(kept, x)
		}
	}
	if len(kept) > arrivalsCap {
		kept = kept[len(kept)-arrivalsCap:]
	}
	return a.store.write(ctx, keyArrivals, kept)
}

// episodeEntries turns arrivals after since into entries, newest first.
func (a *App) episodeEntries(ctx context.Context, client *SiloClient, since time.Time, limit int) ([]Entry, error) {
	list, err := a.arrivals(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].At > list[j].At })
	names := map[string]string{}
	for _, l := range a.tvLibraries(ctx) {
		names[l.ID] = l.Name
	}
	var out []Entry
	for _, x := range list {
		at := time.UnixMilli(x.At)
		if !since.IsZero() && !at.After(since) {
			break
		}
		if e, ok := a.episodeEntry(ctx, client, x.ID, at, LibraryRef{ID: x.Library, Name: names[x.Library]}); ok {
			out = append(out, e)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (a *App) episodeEntry(ctx context.Context, client *SiloClient, id string, at time.Time, lib LibraryRef) (Entry, bool) {
	d := a.detail(ctx, client, id)
	if d == nil {
		return Entry{}, false
	}
	card := d.Card
	card.ContentID = id
	card.Type = "episode"
	card.AddedAt = at.UTC().Format(time.RFC3339Nano)
	if card.ReleaseDate == "" {
		card.ReleaseDate = d.AirDate
	}
	return Entry{Card: card, Library: lib, Episode: d}, true
}

// newestEpisodes returns the episodes with the newest files among the shows
// at the front of each Recently Added row. Previews and tests use it when no
// arrivals have been recorded yet.
func (a *App) newestEpisodes(ctx context.Context, client *SiloClient, n int) []Entry {
	type cand struct {
		id   string
		file int64
		lib  LibraryRef
	}
	var cands []cand
	for _, lib := range a.tvLibraries(ctx) {
		sid, err := client.recentlyAddedSection(ctx, lib.ID)
		if err != nil {
			continue
		}
		series, err := client.sectionSeries(ctx, lib.ID, sid)
		if err != nil {
			continue
		}
		if len(series) > 3 {
			series = series[:3]
		}
		for _, s := range series {
			rows, err := client.seasons(ctx, s.ContentID)
			if err != nil {
				continue
			}
			for _, r := range rows {
				if r.EpisodeCount == 0 {
					continue
				}
				eps, err := client.seasonEpisodes(ctx, s.ContentID, r.SeasonNumber)
				if err != nil {
					continue
				}
				for _, e := range eps {
					if f := e.newestFile(); f > 0 {
						cands = append(cands, cand{id: e.ContentID, file: f, lib: lib})
					}
				}
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].file > cands[j].file })
	now := time.Now()
	var out []Entry
	for i, c := range cands {
		if len(out) >= n {
			break
		}
		// Space the synthetic times so newest-first ordering is kept.
		if e, ok := a.episodeEntry(ctx, client, c.id, now.Add(-time.Duration(i)*time.Second), c.lib); ok {
			out = append(out, e)
		}
	}
	return out
}
