# dropwatch

A small Go web service that shows an artist's discography — grouped so that
deluxe editions, remasters and reissues collapse into one entry — and tracks
which albums/EPs you own, so you can spot what's missing from your library.

Sources: **MusicBrainz** (always on, release-groups are the grouping backbone)
and **Discogs** (optional, merged in by normalized title; catches vinyl-only
and obscure releases). Ownership state lives in a local SQLite database.

## Run

```sh
go build -o dropwatch ./cmd/dropwatch
./dropwatch -addr :8080 -db dropwatch.db \
  -discogs-token xxxx \
  -mpd localhost:6600
```

- `-discogs-token` (or `DISCOGS_TOKEN`) — optional personal access token from
  https://www.discogs.com/settings/developers ("Generate new token", not the
  consumer key). Without it, MusicBrainz only.
- `-mpd` (or `MPD_HOST`, which may be `password@host` or a unix socket path;
  `MPD_PASSWORD` also works) — optional MPD server; enables `/api/sync/mpd`
  and the "sync with mpd" button in the UI.
- `-subsonic` (or `SUBSONIC_URL`) with `SUBSONIC_USER`/`SUBSONIC_PASSWORD` —
  optional Subsonic-compatible server (Navidrome, gonic, Airsonic, ...);
  enables `/api/sync/subsonic` and its UI button. Uses the ID3 endpoints, i.e.
  album artists, with salted token auth.
- Web UI at http://localhost:8080/ — artists with gaps listed first with
  missing counts; search, mark releases owned/ignored, untrack artists.

Release lists are cached in SQLite for 24 h; `refresh=1` (or the UI's Refresh
button) forces a re-fetch. Both sources are rate-limited client-side
(~1 req/s), so the first fetch of a large discography takes a few seconds.

## API

Artist IDs are MusicBrainz MBIDs.

```
GET /api/search/artists?q=<name>
    → {"artists": [{"id", "name", "disambiguation", ...}]}

GET /api/artists                       # tracked artists, most missing first
    → {"artists": [{..., "total": 10, "owned": 2, "missing": 8}]}

DELETE /api/artists/{mbid}             # untrack: removes artist + all state

GET /api/artists/{mbid}/releases
    ?types=album,ep     # default; also: single, all, variants (live/comp/remix)
    &missing=only       # only releases not marked owned/ignored
    &refresh=1          # bypass 24h cache
    → {"artist": {...}, "releases": [{"id", "title", "primaryType",
       "firstReleaseDate", "sources": ["musicbrainz","discogs"],
       "state": "owned"|"ignored"|"", "missing": true|false}]}

PUT /api/releases/{id}/state
    body: {"state": "owned" | "ignored" | ""}   # "" resets to missing

POST /api/sync
    body: {"artists": [{"name": "Boards of Canada",   # or "mbid": "..."
                        "albums": ["Geogaddi",
                                   {"title": "Twoism", "mbid": "<release-group MBID>"}]}]}
    → {"results": [{"artist": {...}, "owned": 3,
        "unmatched": ["titles that matched nothing"],
        "missing": [release groups you don't own]}]}
```

Album entries are either bare title strings or objects with a `mbid` — the
MusicBrainz *release-group* ID (the `MUSICBRAINZ_RELEASEGROUPID` tag written
by Picard/beets). MBID matches are exact and tried first; titles fall back to
the same normalizer the sources use, so `"Music Has the Right to Children
(2013 Remaster)"` matches the plain release group. Artists not seen before
are fetched from the sources during the sync, so the first sync of a big
library takes a while (rate limits); after that everything is cached.

```
POST /api/sync/mpd[?artist=<substring>][&skip_existing=1]
POST /api/sync/subsonic[?artist=<substring>][&skip_existing=1]
    # starts a background pull sync from the configured player server
    → 202 {"started": true}   (409 if one is already running)

GET /api/sync/status
    → {"running", "processed", "total", "current",
       "owned", "missing", "skipped", "failed", "unmatched"}
```

The MPD sync runs as a background job on the server — a first sync of a large
library takes longer than any HTTP request should live, so the POST returns
immediately and progress comes from `/api/sync/status` (the web UI polls it
and survives page reloads). Per-artist detail is in the server log.

`skip_existing=1` (or `"skipExisting": true` in the `/api/sync` body) skips
artists already tracked in the database without touching the sources, making
re-syncs of a large library near-instant. Leave it off when you've ripped new
albums of already-tracked artists and want them marked owned.

The MPD pull uses `MUSICBRAINZ_RELEASEGROUPID` tags when your mpd exposes
them (add the tag to `metadata_to_use` in mpd.conf), falling back to album
titles otherwise. The `?artist=` filter limits the sync to matching album
artists — handy for incremental runs.

### Initial import from a MusicBrainz dump

For a large library, the first sync is faster from a database dump than from
the rate-limited API. Download the **release-group** JSON dump (~1 GB; the
artist dump is not needed — release groups embed their artist credits) from
the newest date directory under
https://data.metabrainz.org/pub/musicbrainz/data/json-dumps/ , then:

```sh
./dropwatch import -dump release-group.tar.xz -mpd localhost:6600 -db dropwatch.db
```

The importer streams the dump once (needs `tar` + `xz`), matches your MPD
album artists against the embedded artist credits, and only imports
**confident matches**: the name matched and at least one of your albums for
that artist exists in the candidate's discography (this is also how same-named
artists are disambiguated). Owned albums are marked in the same pass.
The dump contains no release-level titles, so the importer matches by
normalized group title only. After importing, run **one full sync with "only
new artists" unchecked**: tracked artists resolve locally (no API searches),
and any artist whose albums didn't all match gets refetched live — including
release-title aliases, so reissue-titled rips ("Album: Edition 2004") heal
automatically. Artists the import skipped entirely are resolved by the same
pass via live search. After that, routine syncs with "only new artists"
checked are the cheap default. Discogs data isn't in the dump either; it's
merged in per artist on the next refresh.

### Syncing your local library (push, without -mpd)

From mpd:

```sh
mpc list albumartist | while read -r artist; do
  jq -n --arg a "$artist" \
    '{name: $a, albums: [inputs]}' < <(mpc list album albumartist "$artist" | jq -R .)
done | jq -s '{artists: .}' \
  | curl -s -X POST localhost:8080/api/sync -H 'Content-Type: application/json' -d @- \
  | jq -r '.results[] | .artist.name as $a | .missing[] | "\($a): \(.firstReleaseDate[:4]) \(.title)"'
```

From beets, with release-group MBIDs for exact matching:

```sh
beet ls -a -f '$albumartist\t$album\t$mb_releasegroupid' \
  | jq -Rn '[inputs | split("\t") | {artist: .[0], album: {title: .[1], mbid: .[2]}}]
      | group_by(.artist)
      | {artists: map({name: .[0].artist, albums: map(.album)})}' \
  | curl -s -X POST localhost:8080/api/sync -H 'Content-Type: application/json' -d @-
```

From a `Artist/Album` directory tree:

```sh
find ~/music -mindepth 2 -maxdepth 2 -type d -printf '%P\n' \
  | jq -Rn '[inputs | split("/") | {artist: .[0], album: .[1]}]
      | group_by(.artist)
      | {artists: map({name: .[0].artist, albums: map(.album)})}' \
  | curl -s -X POST localhost:8080/api/sync -H 'Content-Type: application/json' -d @-
```

### Player integration example

Find missing Boards of Canada albums from a script:

```sh
mbid=$(curl -s "localhost:8080/api/search/artists?q=boards+of+canada" \
  | jq -r '.artists[0].id')
curl -s "localhost:8080/api/artists/$mbid/releases?missing=only" \
  | jq -r '.releases[] | "\(.firstReleaseDate[:4]) \(.primaryType) \(.title)"'
```

Mark something as owned (e.g. after your library scanner finds it):

```sh
curl -s -X PUT "localhost:8080/api/releases/$rgid/state" \
  -H 'Content-Type: application/json' -d '{"state":"owned"}'
```

## How grouping works

MusicBrainz *release groups* already collapse all pressings/editions of an
album into one entity. Discogs *masters* do the same on their side. dropwatch
matches Discogs masters to MB release groups by a normalized title
(lowercased, punctuation stripped, edition suffixes like "(Deluxe Edition)" /
"[2017 Remaster]" removed, trailing "EP" dropped) so one album appears exactly
once no matter how many sources or versions carry it. The Discogs artist is
resolved via MusicBrainz's URL relations when linked (reliable), falling back
to Discogs search.

Releases with MusicBrainz *secondary types* (live, compilation, remix, ...)
are treated as variants and hidden by default.
