# Development notes

How the plugin is put together, and how it is deployed on the server it was
built for. No credentials are stored here.

## Files

| File | What it does |
|---|---|
| `main.go` | Serves the plugin through the SDK; wires Configure to `App.configure` |
| `manifest.json` | Capabilities, HTTP routes, admin settings form, presentation |
| `config.go` | Parses the Silo connection and SMTP settings Silo delivers |
| `store.go` | Settings, recipients, per-recipient state and the log, kept in encrypted instance state |
| `engine.go` | The run loop: timers, scan-event trigger, who is due, rate limit, sending |
| `tvdetect.go` | New-episode detection (see below) |
| `silo.go` | Silo REST client: catalog, item detail, images (WebP → JPEG), request pacing |
| `render.go` | Mustache data model and rendering; embeds only the images a template uses |
| `backdrop.go` | Signed public `/public/bg` route serving darkened show backdrops for CSS backgrounds (Gmail ignores `cid:` in CSS) |
| `openlink.go` | Public `/public/open` page that opens an item in the Silo app (`silo://item/<id>`, Android intent with web fallback) or on the web |
| `templates.go` | Default subject, HTML and text templates, and the confirmation email |
| `mailer.go` | SMTP sending through go-mail |
| `http.go` | Admin API, self-service API and the public verify/unsubscribe/manage pages |
| `events.go` | Event consumer (`library.item_added`, …) and the scheduled task |
| `web/admin.html`, `web/me.html` | Admin and user pages (vanilla JS, embedded) |
| `*_test.go` | Unit tests; `live_*_test.go` run against a real server with `-tags live` |

## Things that aren't obvious

- **Episodes can't be found by `added_at`.** Silo creates episode items from
  series metadata before their files exist, so a newly downloaded episode
  keeps an old date. `tvdetect.go` reads the front of each TV library's
  "Recently Added TV" row (ordered by arrival), compares each show's
  per-season `episode_count` with the last check, and treats episodes whose
  file id is above a watermark as arrivals. Upgrades don't change the count,
  so they don't notify. Arrivals are logged in instance state for 9 days.
- **The Recently Added TV row mixes shows and episodes.** A show with a
  single new episode appears as that episode's card (`type: "episode"`,
  `series_id`), not as the show. `rowSeries` maps those to their series;
  before 1.5.2 they were looked up as series, returned 404 and were missed.
- **Movies can use `added_at`**: a movie item is created when its file is
  scanned. Checked against the Recently Added Movies row.
- **Catalog requests need `X-Profile-Id`**, even for an admin API key. The
  plugin uses the key owner's primary profile unless one is configured.
- **Rate limit.** An API key shares its owner's allowance (standard tier: 20
  requests a second, burst 20). `silo.go` paces requests to 8 a second and
  honours `Retry-After`.
- **Artwork is WebP** and presigned for a short time, so images are fetched,
  converted to JPEG and embedded as `cid:` parts. Item details are cached
  for a few minutes, so `refreshArtwork` re-reads them just before each send
  (and the backdrop route retries with a fresh detail); before 1.5.3 expired
  cached URLs made images silently disappear. A failed download is logged as
  "artwork download failed".
- **Backdrop backgrounds are linked, not embedded.** `{{show_backdrop_bg}}`
  points at the plugin's public `/public/bg` route (HMAC-signed with a secret
  in instance state), so the plugin must be running when the email is opened;
  if it isn't, the card colour shows instead.
- **Item links open the app.** `{{url}}` and `{{show_url}}` point at
  `/public/open`, because mail clients don't link `silo://` URLs and a
  self-hosted domain can't carry the apps' universal links. `{{web_url}}` and
  `{{show_web_url}}` go straight to the web. Turned off with the Design tab's
  "Open links in the Silo app" switch.
- **Saved templates override the defaults.** Changing `templates.go` only
  affects new installs and "Reset to default"; update the saved templates
  through `PUT /api/admin/settings` as well.
- **Uploads replace in place.** Uploading a binary with the same `plugin_id`
  keeps the installation id, configuration and instance state.
- **The manifest needs `supported_platforms`** or Silo refuses the upload.

## Build, test, deploy

```sh
export PATH=$HOME/.local/go/bin:$PATH   # Go 1.26+
make test
make build          # dist/silo-plugin-email-alerts-linux-amd64
make build-arm64    # dist/silo-plugin-email-alerts-linux-arm64
```

Bump `version` in `manifest.json` and `VERSION` in the Makefile, then upload
through Admin → Plugins → Upload, or:

```sh
curl -H "Authorization: Bearer $SILO_KEY" -H "X-Profile-Id: $PROFILE" \
  -F "archive=@dist/silo-plugin-email-alerts-linux-amd64" \
  http://localhost:8098/api/v2/admin/plugins/uploads
```

The plugin's own admin API is at
`/api/v2/plugin-content/plugins/<installation id>/api/admin/` (`state`,
`settings`, `preview`, `test`, `run`, `recipients`). It accepts the same
API key and profile headers.

## The original deployment

- Silo at `http://localhost:8098` (public `https://silo.locky.uk`), installed
  as installation 5. Built against `silo-plugin-sdk` v0.30.0.
- Sonarr (`:8989`) and Radarr (`:7878`) each have a "Silo Autoscan" webhook
  pointing at Silo's built-in webhook sources, so Silo scans seconds after an
  import. The `silo.autoscan.arr` plugin (installation 6) polls their history
  every 10 minutes as a fallback.
- Current settings: instant alerts, 0-minute wait after a scan, 10-minute
  minimum gap between instant emails, Europe/London, no Watch now buttons
  (titles are links).
