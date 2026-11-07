# Silo Filmography plugin

Adds a **Filmography** page to Silo's menu (every signed-in user). Search for
any actor or crew member to see every film and TV credit from TMDB, with
titles in the library marked and linked into Silo, a full-size photo gallery
in a lightbox, and links to TMDB, IMDb, Wikipedia, Wikidata and social pages.

Plugins can't add to Silo's own actor screen, so this is a separate page.
Deep links: `/filmography/?person=<TMDB id>`, `?silo=<Silo person id>`, and
the search box accepts a pasted TMDB person URL or Silo person id.

When the page is opened from a Silo actor page (the browser's referrer is
`/person/<id>` on the same origin), the first load shows that person.

## Files

| File | What it does |
|---|---|
| `main.go` | Serves the plugin (HTTP routes only) |
| `app.go` | Config, Silo client setup, library matching, caches |
| `tmdb.go` | TMDB client; merges cast and crew into one credit per title; self-appearance detection; person links |
| `silo.go` | Silo REST client (shared with the theme-songs plugin) plus person lookups |
| `http.go` | `/filmography/` page and `/api/search`, `/api/person/<id>`, `/api/silo-person/<id>` |
| `web/filmography.html` | The page (vanilla JS; images load straight from image.tmdb.org) |

## Things that aren't obvious

- **Library matching** finds the Silo person by name search plus matching
  `tmdb_id`, then lists their titles with `catalog?source=person`. Movie ids
  carry the TMDB id (`movie-tmdb-862`); series (`series-tvdb-…`) are looked up
  once and remembered. Silo only credits the cast it stores, so a minor role in
  a library title may not be marked.
- **Self** marks a title whose acting roles are all appearances as themselves
  (talk/news/reality genres, or a character like "Self - Narrator"). Crew
  credits on the same title don't clear it; the page hides self titles under
  Acting unless "Include appearances as self" is ticked.
- TMDB answers are cached 6 h (people) and 30 min (searches); library matches 5 min.
- A v4 read access token (JWT) is sent as a Bearer header; a v3 key as `api_key`.

## Build, test, deploy

```sh
export PATH=$HOME/.local/go/bin:$PATH
make test
go test -tags live -run Live ./...   # needs ~/.tmdb-key and ~/.silo-key
make build
curl -H "Authorization: Bearer $SILO_KEY" -H "X-Profile-Id: $PROFILE" \
  -F "archive=@dist/silo-plugin-filmography-linux-amd64" \
  http://localhost:8098/api/v2/admin/plugins/uploads
```

Bump `version` in `manifest.json` and `VERSION` in the Makefile first.
