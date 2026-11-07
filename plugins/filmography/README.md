# Silo Filmography plugin

Adds a **Filmography** page to Silo's menu (every signed-in user). Search for
any actor or crew member to see every film and TV credit from TMDB, with
titles in the library marked and linked into Silo, a Request button on the
rest, a full-size photo gallery in a lightbox, and links to TMDB, IMDb,
Wikipedia, Wikidata and social pages.

This is BEisem's fork of lockyuk's plugin (plugin id `beisem.filmography`).
It differs from the original in three ways:

- **No Silo API key.** Library titles are found through Silo's plugin host
  (`CheckMediaPresence` and `ListLibraries`), so the only credential the
  plugin holds is a TMDB key.
- **Request button.** Titles that aren't in the library link to Silo's own
  title page (`/title/movie/<id>`, `/title/series/<id>`), where Silo offers
  the request and applies its own permissions, approval and limits.
- **Back to Silo.** Plugin pages open as a full page, outside Silo's layout,
  so the top bar has a link back to the Silo page you came from.

Plugins can't add to Silo's own actor screen, so this is a separate page.
Deep link: `/filmography/?person=<TMDB id>`. The search box also accepts a
pasted TMDB person URL.

## Files

| File | What it does |
|---|---|
| `main.go` | Serves the plugin (HTTP routes only) |
| `app.go` | Config, library matching through Silo's plugin host, caches |
| `tmdb.go` | TMDB client; merges cast and crew into one credit per title; self-appearance detection; person links |
| `http.go` | `/filmography/` page with its Content-Security-Policy, and `/api/search`, `/api/person/<id>` |
| `web/filmography.html` | The page (vanilla JS; images load straight from image.tmdb.org) |

## Security notes

- **Who is viewing.** Silo stamps `X-Silo-User-Id` on every request to a
  plugin route after checking the session, and forwards only a short
  allowlist of browser headers, so a browser can't set it. Without it, no
  titles are marked. `ListLibraries` lists every library when given no user,
  so the plugin never calls it without one.
- **What is marked.** A title is marked only when it's in a library that user
  may see. Profile restrictions (ratings limits) aren't applied to the marks;
  opening a restricted title is still blocked by Silo.
- **Requests.** The plugin never creates requests itself. The button only
  links to Silo's title page, so a request is made by the signed-in user under
  Silo's rules.
- **The page.** It is served from Silo's own origin, so it carries a strict
  CSP in a `<meta>` tag: only its one script runs (allowed by SHA-256 hash,
  computed when the plugin starts), it can only call its own origin, and
  images load only from image.tmdb.org. TMDB text is escaped before it's
  rendered, and image paths that don't look like TMDB file names are dropped.
- **Back link.** Only a referring page on Silo's origin, outside the plugin,
  is used; anything else goes to Silo's home page.

## Things that aren't obvious

- **Library matching** sends a person's TMDB ids to Silo in batches of 100.
  Silo checks each title's TMDB id (and the TVDB/IMDb ids it has for it), so
  every title in the library is marked, not only ones where Silo stored that
  person in the cast.
- When a title is in several libraries, Silo reports one of them. If that one
  is hidden from the viewer, the title shows as not in the library.
- **Self** marks a title whose acting roles are all appearances as themselves
  (talk/news/reality genres, or a character like "Self - Narrator"). Crew
  credits on the same title don't clear it; the page hides self titles under
  Acting unless "Include appearances as self" is ticked.
- TMDB answers are cached 6 h (people) and 30 min (searches); library matches
  and each user's library list 5 min.
- A v4 read access token (JWT) is sent as a Bearer header; a v3 key as `api_key`.

## Build, test, release

The workflow in `.github/workflows/filmography.yml` runs `gofmt`, `go vet`,
`go test` and both builds on every push. On `main` it also publishes a GitHub
release for a version that hasn't been released and commits the updated
catalog `manifest.json` at the repository root.

To release: bump `version` in `manifest.json` and `VERSION` in the Makefile,
then merge to `main`.

Locally, with Go 1.26:

```sh
make test
make build
```
