# dropwatch

dropwatch answers one question: **which albums am I missing?**

It keeps a list of your artists, knows every album and EP they ever released
(via MusicBrainz, optionally Discogs), compares that against your music
library, and shows you the gaps. Different versions of the same album —
remasters, deluxe editions, reissues — count as one album, so you don't get
told you're "missing" a record you already own in a different pressing.

## Quick start

```sh
go build -o dropwatch ./cmd/dropwatch
./dropwatch -mpd localhost:6600 -discogs-token XXXX
```

Open http://localhost:8080. Everything works without flags too — you just
lose the extras:

| Flag / env var | What it enables |
|---|---|
| `-mpd` / `MPD_HOST` | syncing straight from MPD. Accepts `host`, `host:port`, `password@host`, or a socket path. `MPD_PASSWORD` works too. |
| `-subsonic` / `SUBSONIC_URL` + `SUBSONIC_USER`, `SUBSONIC_PASSWORD` | syncing from Navidrome, gonic, Airsonic, … |
| `-discogs-token` / `DISCOGS_TOKEN` | Discogs as a second source — catches vinyl-only and obscure releases. Free token: discogs.com/settings/developers → "Generate new token". |
| `-db`, `-addr` | database path and listen address. |
| `-dump-dir` | temporary download directory for scheduled release checks; defaults to `<db path>.dumps`. |

## Getting your library in

**Small library?** Just click "sync mpd" (or subsonic) in the web UI. Every
artist is looked up live; expect a few seconds per artist because MusicBrainz
and Discogs are rate-limited.

**Large library?** Do the initial import from MusicBrainz database dumps
instead — minutes instead of hours. From the newest dated directory at
https://data.metabrainz.org/pub/musicbrainz/data/json-dumps/ grab:

- `release-group.tar.xz` (~1 GB) — required
- `release.tar.xz` — optional but worth it: it knows the title of every
  *version* of every album, so a rip named "Aaliyah: Edition 2004" is
  recognized as the album "Aaliyah" and marked owned right away

```sh
./dropwatch import -dump release-group.tar.xz -release-dump release.tar.xz \
  -mpd localhost:6600
```

Both files can also be given pre-extracted, which skips the slow xz
decompression. The importer only keeps artists it's *sure* about: the name
must match and at least one of your albums must appear in that artist's
discography (that's also how two artists with the same name are told apart).
Everything it wasn't sure about is printed at the end.

Then, once: run a sync from the web UI with "only new artists" **unchecked**.
This picks up the artists the import skipped and double-checks the rest
against the live API. It's cheap — artists already in the database don't
cause any API traffic unless something doesn't match. After that, day-to-day
syncs with "only new artists" checked are near-instant.

## The web UI

- **artists** — your artists, with owned/total counts and how many albums
  are missing. Filter as you type. Artists with gaps are listed first.
- **library** — everything you own, filterable and sortable, with toggles
  for albums / EPs / other.
- **lookup** — search MusicBrainz to start tracking an artist you don't have
  locally yet. This is the only place that searches the outside world.
- **notifications** — new and upcoming releases, with an unread count and
  release-day reminders.
- **settings** — automatic dump downloads: selected weekdays (or every day),
  time of day, and timezone. Includes the next check, progress, errors, and
  a **check now** button.

On an artist page you can mark any release owned or ignored, show variants
(live albums, compilations), re-fetch from the sources, sync just this one
artist from your player, or untrack the artist. Anything you own is always
visible, whatever the filters say.

## Scheduled release notifications

Open **settings**, enable automatic downloads, choose the days and time,
and save. For example, select **Thursday and Sunday at 04:00** in
**Europe/Berlin**. MusicBrainz [publishes JSON dumps twice a week](https://metabrainz.org/datasets/postgres-dumps),
on Wednesdays and Saturdays; the following morning leaves time for publication.
Automatic downloads are off until enabled. Settings persist in the database.

Only `release-group.tar.xz` is downloaded (about 1 GB). Dropwatch checks the
published version first and skips an already processed dump, unless newly
tracked artists need their first scan. It verifies the SHA-256 checksum,
streams the dump with `tar` and `xz`, and removes the temporary download
afterward. Allow space for one compressed dump in `-dump-dir`.

Each artist's first successful scan establishes a silent baseline. Subsequent
scans notify about new canonical albums, EPs and singles released within the
last 30 days or due in the future. Older additions and undated records still
update the discography but do not produce notifications. A known upcoming
release with an exact date gets a release-day reminder at midnight in the
configured timezone, including upcoming releases in the initial baseline.
Partial dates never invent a release day. Notifications appear in the web UI;
no email or external push service is required or contacted.

The updater matches artists by MusicBrainz ID, preserves ownership, aliases
and Discogs metadata, and commits only complete scans. Reprocessing the same
dump does not duplicate notifications. While scheduling is enabled, artists
with a dump baseline use cached discographies when opened; **refresh sources**
still explicitly fetches live data.

Dropwatch must be running at the scheduled time. A missed run is caught up
once on startup; failed checks retry at the next scheduled time or via
**check now**. An active check finishes if scheduling is disabled. Named
timezones follow daylight saving time; a nonexistent spring-transition time
is skipped and an autumn overlap runs once. `Local` means the server's timezone.

## Using it from scripts

Artist IDs are MusicBrainz IDs. All responses are JSON.

```
GET  /api/search/artists?q=name          search MusicBrainz
GET  /api/artists                        tracked artists with counts
GET  /api/artists/{id}/releases          one artist's releases
       ?missing=only  ?types=all  ?refresh=1
DELETE /api/artists/{id}                 untrack
GET  /api/library                        everything owned
PUT  /api/releases/{id}/state            {"state": "owned" | "ignored" | ""}
POST /api/sync                           push your library as JSON (see below)
POST /api/sync/mpd                       pull from MPD (background job)
POST /api/sync/subsonic                  pull from Subsonic (background job)
       ?artist=name  ?exact=1  ?skip_existing=1
GET  /api/sync/status                    progress of the running sync
GET  /api/dumps                          dump schedule and download status
PUT  /api/dumps/schedule                 {"enabled":true,"days":[0,4],"time":"04:00","timezone":"Europe/Berlin"}
POST /api/dumps/check                    check/download the latest dump now
GET  /api/notifications                  latest 200 notifications and unread count
POST /api/notifications/read             mark all currently due notifications read
```

Find missing albums from a script:

```sh
mbid=$(curl -s "localhost:8080/api/search/artists?q=boards+of+canada" | jq -r '.artists[0].id')
curl -s "localhost:8080/api/artists/$mbid/releases?missing=only" \
  | jq -r '.releases[] | "\(.firstReleaseDate[:4]) \(.title)"'
```

Push a library without MPD/Subsonic — album entries are either plain titles
or `{"title": ..., "mbid": "<release-group id>"}`:

```sh
curl -s -X POST localhost:8080/api/sync -H 'Content-Type: application/json' \
  -d '{"artists": [{"name": "Boards of Canada", "albums": ["Geogaddi", "Twoism EP"]}]}'
```

## How matching works (short version)

MusicBrainz already groups all versions of an album into one "release group";
Discogs does the same with "masters". dropwatch merges the two by normalized
title, and drops Discogs-only entries that turn out to be bootlegs, promos,
or repackaged box sets. Your album titles are matched forgivingly — case,
punctuation and edition suffixes like "(Deluxe Edition)" or ": Edition 2004"
don't matter — and against the title of every known *version* of each album,
plus MusicBrainz IDs from your tags when present. If something still doesn't
match against cached data, the sync re-fetches that artist once and tries
again, so stale data heals itself.
