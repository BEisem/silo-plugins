# silo-plugins

A plugin repository for [Silo](https://github.com/Silo-Server). Add this URL in
**Admin → Plugins → Repositories**:

```
https://raw.githubusercontent.com/lockyuk/silo-plugins/main/manifest.json
```

| Plugin | What it does |
|---|---|
| [`google-auth`](plugins/google-auth) | Sign in to Silo with Google (OAuth 2.0 + PKCE). Anyone with an enabled Silo account may use the Google address on it. |
| [`email-alerts`](plugins/email-alerts) | Emails customisable alerts for new episodes and movies, with instant, daily and weekly modes. |

Binaries (linux/amd64 and linux/arm64) are attached to each release; the catalog
`manifest.json` lists their SHA-256 checksums, which Silo verifies on install.

## Releasing

1. Bump `version` in the plugin's `manifest.json` and `VERSION` in its `Makefile`.
2. `make test` in the plugin directory.
3. `scripts/build.py --release` builds both architectures, creates the GitHub
   releases (`<plugin>-v<version>`), and rewrites `manifest.json`.
4. Commit and push `manifest.json`.

Release a version before pushing the index that lists it, or Silo will see a
catalog entry with no download.

## Trust

Plugins run as native processes inside your Silo container with access to what
Silo gives them. Only add repositories you trust, and read the source of a
plugin before installing it. Everything here is MIT licensed.
