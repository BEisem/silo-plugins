# silo-plugins

BEisem's fork of [lockyuk/silo-plugins](https://github.com/lockyuk/silo-plugins).
This fork publishes one plugin, `beisem.filmography`; see
[plugins/filmography](plugins/filmography) for what changed. The other plugin
folders are the upstream copies and aren't built or listed here.

Add this URL in **Admin → Plugins → Repositories**:

```
https://raw.githubusercontent.com/BEisem/silo-plugins/main/manifest.json
```

| Plugin | What it does |
|---|---|
| [`google-auth`](plugins/google-auth) | Sign in to Silo with Google (OAuth 2.0 + PKCE). Anyone with an enabled Silo account may use the Google address on it. |
| [`filmography`](plugins/filmography) | A Filmography page: any actor's full TMDB credits with the titles in your library marked and linked. Opens on the actor you came from when launched from their page. |
| [`email-alerts`](plugins/email-alerts) | Emails customisable alerts for new episodes and movies, with instant, daily and weekly modes. |

Binaries (linux/amd64 and linux/arm64) are attached to each release; the catalog
`manifest.json` lists their SHA-256 checksums, which Silo verifies on install.

## Releasing

Bump `version` in the plugin's `manifest.json` and `VERSION` in its `Makefile`,
then merge to `main`. The Filmography workflow tests and builds both
architectures, creates the GitHub release (`filmography-v<version>`), and
commits the updated `manifest.json`.

## Trust

Plugins run as native processes inside your Silo container with access to what
Silo gives them. Only add repositories you trust, and read the source of a
plugin before installing it. Everything here is MIT licensed.
