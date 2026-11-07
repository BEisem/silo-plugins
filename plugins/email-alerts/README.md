# Email Alerts plugin for Silo

Emails new episodes and movies from anywhere on the server, not just series a
profile follows. Templates, design and recipients are edited on an admin
page with a live preview.

- Instant alerts a few minutes after each scan, daily or weekly digests, or
  instant alerts plus a daily digest, chosen per recipient
- Editable subject, HTML and plain-text templates using Mustache variables
  such as `{{show_name}}`, `{{episode_name}}` and `{{show_synopsis}}`
- Posters and episode stills embedded in the email, so they show even when
  the reader can't reach your server
- Admin-managed recipients, plus a self-service page in every user's
  sidebar with address confirmation
- A manage link and one-click unsubscribe in every email

## Install

1. Build it, or use the binary in `dist/`:

   ```sh
   make build        # writes dist/silo-plugin-email-alerts-linux-amd64
   ```

2. In Silo, open **Admin → Plugins**, choose **Upload**, and pick
   `dist/silo-plugin-email-alerts-linux-amd64`. Enable the plugin.
3. Open the plugin's settings:
   - **Silo connection:** an API key from an admin account (Settings → API
     keys, unscoped). The plugin reads new items from the catalog with it,
     acting as the key owner's primary profile unless you set a Profile ID
     under Advanced.
   - **Outgoing mail (SMTP):** the same details as Admin Settings →
     Notifications → Email.
4. In **Admin → Scheduled tasks**, give **Email alerts: send due emails** a
   15-minute interval. It runs at startup by default; the interval keeps
   digests on time even when no scan happens.
5. Open **Email alerts** in the admin menu. Send a test, edit the templates,
   and add recipients.

## How it decides what's new

Silo tells the plugin when a scan adds items (`library.item_added`). After
the settle delay (configurable; 0 sends straight away) and a 5-second batching
window, the plugin reads the newest movies and episodes from every movie and
TV library, sorted by date added. It also checks every minute while running,
which catches files that were matched to an episode some time after they
arrived.

Each recipient keeps a watermark (the newest date added they've been
considered for) and a list of recently sent items, so a retried run never
sends the same item twice. New recipients start from the moment they're
added rather than receiving the whole back catalogue. Use **Resend 24h** on
the Recipients tab to send recent items again.

Instant alerts are rate limited per recipient (Schedule & sign-up tab). With
the default 10-minute gap, the first new item goes out straight away and
anything arriving in the next 10 minutes is held and sent together, so a
whole season downloading doesn't send an email per episode. An optional daily
cap holds anything past it for the first email after midnight. Held items
are never dropped.

The **Instant + daily digest** mode sends instant alerts as usual and, at the
daily digest hour, a recap of everything added that day, including items an
instant alert already covered. The two are tracked separately, so the instant
rate limit never holds back the digest.

**Count as new if added within** (Schedule & sign-up tab, default 1 week) is
the oldest an item can be and still be emailed. Each recipient can pick their
own window (24 hours to 30 days) on their sign-up page, the Manage alerts
link, or the recipient dialog. It stops a long outage or a stale state from
sending the back catalogue as "new". Weekly digests need at least 1 week.

A failed send backs off, from 2 minutes up to about an hour, and the error
appears in the send log.

## Templates

Templates use [Mustache](https://mustache.github.io/mustache.5.html):

| Syntax | Meaning |
|---|---|
| `{{name}}` | Insert a value, HTML-escaped |
| `{{{name}}}` | Insert a value without escaping |
| `{{#shows}}…{{/shows}}` | Repeat for each show; or show the block when a value is set or true |
| `{{^has_movies}}…{{/has_movies}}` | Show the block when a value is empty or false |
| `{{! note }}` | Comment |

The **Variables** pane in the editor lists every variable and inserts it on
click. The main ones:

- **Whole email:** `server_name`, `server_url`, `date`, `summary` ("3 new
  episodes across 2 shows and 1 new movie"), `total_count`, `movie_count`,
  `episode_count`, `show_count`, `more_count`, `recipient_name`,
  `recipient_first_name`, `manage_url`, `unsubscribe_url`
- **Conditions:** `has_shows`, `has_movies`, `single_item`, `has_more`,
  `is_instant`, `is_digest`, `is_daily`, `is_weekly`
- **`{{#shows}}`:** `show_name`, `show_synopsis`, `show_poster`,
  `show_backdrop`, `show_genres`, `network`, `show_rating`,
  `episodes_label`, `episode_range`, `first_episode_image`, `first_url`, and
  the nested `{{#episodes}}` list
- **`{{#episodes}}`:** `episode_name`, `episode_code` (S02E03),
  `season_number`, `episode_number`, `episode_synopsis`, `episode_image`,
  `air_date`, `runtime`, `url`
- **`{{#movies}}`:** `title`, `year`, `synopsis`, `tagline`, `genres`,
  `rating`, `content_rating`, `runtime`, `poster`, `backdrop`, `cast`, `url`
- **Theme:** `accent_color`, `background_color`, `card_color`,
  `text_color`, `muted_color`, `logo_url`, `header_title`, `intro`,
  `footer_text`, `button_text`
- **Custom variables** you add on the Design tab, as `{{key}}` and
  `{{custom.key}}`

Synopses are shortened to the length set on the Design tab; the `_full`
variants (`show_synopsis_full`, `episode_synopsis_full`, `synopsis_full`)
are never shortened.

Image variables hold `cid:` references to images embedded in the email. Only
images the template actually uses are downloaded and attached, up to the
per-email limit. An image that can't be fetched is dropped and the email is
rendered again, so `{{#show_poster}}…{{/show_poster}}` blocks disappear
rather than show a broken image.

## Where things are stored

Connection and SMTP settings are the plugin's Silo configuration (encrypted
by Silo). Templates, recipients, per-recipient progress and the send log are
in the installation's encrypted instance state. Nothing is written to disk.

## Development

```sh
make test
make build
```

Requires Go 1.26 or newer. Built against `silo-plugin-sdk` v0.30.0.
