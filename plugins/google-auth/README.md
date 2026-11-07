# silo-plugin-google-auth

Sign in to Silo with Google. An `auth_provider.v1` plugin (OAuth 2.0 authorization
code flow with PKCE and a nonce).

## Rules

- A Google sign-in is accepted when Google reports a **verified email** that
  belongs to an **enabled Silo account**. The plugin asks Silo with a read-only
  API key (`admin:monitor:read`); there are no email or domain lists here.
- The plugin never sets a role. Silo keeps the role the account already has.
- Silo links the Google identity (keyed on Google's immutable `sub`) to the
  account. Turn on Silo's `auth.email_auto_match` setting to do that on the first
  sign-in; otherwise users link Google in their account settings. Auto-match
  never links admin, owner or break-glass accounts.
- Every failure is closed: no API key, a failed Silo lookup or a Google outage
  refuses the sign-in (as `PROVIDER_UNAVAILABLE`), and never revokes anyone.
- The periodic account re-check (`CheckAccount`) only refreshes the Google
  token and confirms the same Google account and a verified email. Silo itself
  revokes sessions of disabled or deleted accounts.

## Settings

Client ID, client secret (Google Cloud Console, Web application client), Silo
API key, and an optional default `prompt` (`select_account`, `login`, `consent`,
`none`). Redirect URI to register in Google:
`https://<silo host>/api/v2/auth/oauth/<installation id>/callback`.

## Security notes

- The ID token's signature is not verified. It is received directly from
  Google's token endpoint over TLS in exchange for the client secret, which
  OpenID Connect Core 3.1.3.7 allows. Issuer, audience, expiry and nonce are
  checked.
- The Silo URL is fixed to `http://127.0.0.1:8080` (the listener in the same
  container). If Silo's internal port changes, rebuild the plugin.

## Build and deploy

```sh
export PATH=$HOME/.local/go/bin:$PATH   # Go 1.26+
make test
make build   # dist/silo-plugin-google-auth-linux-amd64
```

Bump `version` in `manifest.json` and `VERSION` in the Makefile, then upload:

```sh
curl -H "Authorization: Bearer $SILO_KEY" -H "X-Profile-Id: $PROFILE" \
  -F "archive=@dist/silo-plugin-google-auth-linux-amd64" \
  http://localhost:8098/api/v2/admin/plugins/uploads
```

Uploading the same `plugin_id` keeps the installation id and settings.
Settings and enabling go through `PUT /api/v2/admin/plugins/installations/<id>/config`
(entry key `google`) and `PUT .../auth-binding`; test with `POST .../auth-binding/test`.
