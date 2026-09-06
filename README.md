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

On an artist page you can mark any release owned or ignored, show variants
(live albums, compilations), re-fetch from the sources, sync just this one
artist from your player, or untrack the artist. Anything you own is always
visible, whatever the filters say.

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
