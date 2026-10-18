# dropwatch

dropwatch answers one question: **which albums am I missing?**

It keeps a list of your artists, knows every album and EP they ever released
(via MusicBrainz, optionally Discogs), compares that against your music
library, and shows you the gaps. Different versions of the same album —
remasters, deluxe editions, reissues — count as one album, so you don't get
told you're "missing" a record you already own in a different pressing.

## Quick start

Download a Linux binary from the
[releases page](https://github.com/carnager/dropwatch/releases), or build it
(Go 1.27+, no C compiler needed):

```sh
go build -o dropwatch ./cmd/dropwatch
./dropwatch
```

`tar` and `xz` must be installed; dropwatch uses them to read MusicBrainz
dumps.

Open http://localhost:8080 and everything else happens in the browser:

1. **Set a password.** The first visit asks for one; after that the UI and
   the API need a login (or an API token, see below).
2. **Settings → Library.** Enable any of MPD, Subsonic (Navidrome, gonic,
   Airsonic, …) and melody, fill in address and credentials, and use
   **test connection**. Several sources can be on at once: a sync reads all
   of them, and an album owned in any one counts as owned. Optionally paste
   a Discogs token (discogs.com/settings/developers → "Generate new token")
   to catch vinyl-only and obscure releases.
3. **Settings → Release checks → check now.** Downloads the MusicBrainz
   release-group dump (about 1 GB) and keeps it.
4. **sync library** (top right). New artists are resolved from that dump in
   one pass — a couple of minutes even for thousands of artists — and only
   the few it can't confirm are looked up online.

Passwords and tokens are stored in the database and never shown again in the
UI; leave a field empty to keep the saved value.

Only three flags remain: `-addr` (listen address, default `:8080`), `-db`
(database path) and `-dump-dir` (where the dump is kept, default
`<db path>.dumps`). The older source flags and environment variables
(`-mpd`/`MPD_HOST`, `-subsonic`/`SUBSONIC_*`, `-melody`/`MELODY_*`,
`-discogs-token`/`DISCOGS_TOKEN`) are read once, when a database is created,
so an existing setup carries over; after that the web UI is in charge.

### NixOS and Home Manager

The repository is a flake with a package, a NixOS module and a Home Manager
module (Linux, x86_64 and aarch64):

```nix
{
  inputs.dropwatch.url = "github:carnager/dropwatch";

  outputs = { nixpkgs, dropwatch, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        dropwatch.nixosModules.default
        {
          services.dropwatch = {
            enable = true;
            address = "0.0.0.0";        # default 127.0.0.1
            port = 8080;
            openFirewall = true;
            # environmentFile = "/run/secrets/dropwatch.env";
          };
        }
      ];
    };
  };
}
```

For a user service, import `dropwatch.homeManagerModules.default` into your
Home Manager configuration instead; it takes the same options except
`openFirewall`, `user` and `group`, and keeps its data under
`~/.local/share/dropwatch`.

| Option | Default | |
|---|---|---|
| `enable` | `false` | |
| `package` | this flake's package | |
| `address`, `port` | `127.0.0.1`, `8080` | `-addr`; IPv6 addresses are bracketed for you |
| `dataDir` | `/var/lib/dropwatch` | |
| `database` | `<dataDir>/dropwatch.db` | `-db` |
| `dumpDir` | `<dataDir>/dumps` | `-dump-dir`; room for ~2.5 GB |
| `environment` | `{}` | extra environment variables (end up in the Nix store) |
| `environmentFile` | `null` | `NAME=value` file read by systemd, for secrets |
| `extraArgs` | `[]` | more command-line arguments |
| `openFirewall` | `false` | NixOS only |
| `user`, `group` | `dropwatch` | NixOS only; created when left at the default |

Everything else (library sources, Discogs, schedules) is set in the web UI.
The legacy variables (`MPD_HOST`, `MELODY_SERVER`, `DISCOGS_TOKEN`, …) in
`environment` or `environmentFile` only seed those settings when the
database is created. The NixOS service runs with systemd sandboxing
(read-only system, no home directories), so a password file referenced by
`MELODY_PASSWORD_FILE` must live outside `/home`; prefer `MELODY_PASSWORD`
in `environmentFile`.

## Getting your library in

A sync reads every enabled source and works out which MusicBrainz artist each
of your album artists is. For an artist it doesn't know yet it tries, in
order: a name or alias it has seen before; for "A & B" credits, artist A if
one of the albums is in A's discography; the kept dump (name plus at least
one matching album); the credited artist of one of your tagged albums (one
MusicBrainz request, exact); and finally a name search that only accepts an
artist actually called that. Various Artists is never accepted.

With **only new** checked, known artists cause no requests at all — their
albums are matched against the database, so new purchases still get marked
owned. Uncheck it occasionally to re-verify everything against MusicBrainz.

**Release titles (optional).** Settings → Release titles downloads the
MusicBrainz release dump (about 21 GB) on the release-check schedule, at most
as often as you choose. It streams straight through without being stored and
keeps only the titles and release IDs of albums already in dropwatch. That
makes albums tagged with a release ID (Subsonic, melody) and reissue-titled
rips ("Aaliyah: Edition 2004") match exactly.

The command-line importer (`dropwatch import -dump release-group.tar.xz
[-release-dump release.tar.xz] -mpd host`) still exists for scripted setups.

## The web UI

- **artists** — your artists, with owned/total counts and how many albums
  are missing. Filter as you type. Artists with gaps are listed first.
- **library** — everything you own, filterable and sortable, with toggles
  for albums / EPs / other.
- **lookup** — search MusicBrainz to start tracking an artist you don't have
  locally yet. This is the only place that searches the outside world.
- **notifications** — new and upcoming releases, with an unread count,
  album/EP/single filters and release-day reminders. Mark them read one at
  a time (opening one does too) or all at once.
- **settings** — three tabs: *library* (sources with live connection
  status, Discogs), *musicbrainz* (dump status with **check now**, the
  download schedule, optional release titles) and *account* (password,
  API token, log out).

On an artist page you can mark any release owned or ignored, show variants
(live albums, compilations), re-fetch from the sources, sync just this one
artist from your library, or untrack the artist. Anything you own is always
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
streams the dump with `tar` and `xz`, and keeps the latest verified copy for
resolving new artists during syncs. Allow space for two compressed dumps in
`-dump-dir` (the kept one and a download in progress).

Each artist's first successful scan announces its releases from the last 30
days. Subsequent scans notify about new canonical albums, EPs and singles released within the
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

Artist IDs are MusicBrainz IDs. All responses are JSON. Create an API token
under settings → Account and send it with every request:

```sh
export DW="Authorization: Bearer <token>"
curl -s -H "$DW" localhost:8080/api/artists
```

```
GET  /api/search/artists?q=name          search MusicBrainz
GET  /api/artists                        tracked artists with counts
GET  /api/artists/{id}/releases          one artist's releases
       ?missing=only  ?types=all  ?refresh=1
DELETE /api/artists/{id}                 untrack
GET  /api/library                        everything owned
PUT  /api/releases/{id}/state            {"state": "owned" | "ignored" | ""}
POST /api/sync                           push your library as JSON (see below)
POST /api/sync/library                   pull from all enabled sources (background job)
       ?artist=name  ?exact=1  ?skip_existing=1
GET  /api/sync/status                    progress of the running sync
GET  /api/dumps                          dump schedules and download status
PUT  /api/dumps/schedule                 {"enabled":true,"days":[0,4],"time":"04:00","timezone":"Europe/Berlin"}
POST /api/dumps/check                    check/download the latest release-group dump now
PUT  /api/dumps/releases                 {"enabled":true,"intervalDays":30}
POST /api/dumps/releases/check           download release titles now
GET  /api/notifications                  latest 200 notifications and unread count
       ?types=Album,EP,Single
POST /api/notifications/read             mark due notifications read (same ?types=)
POST /api/notifications/{id}/read        mark one notification read
GET  /api/settings, PUT /api/settings    sources and Discogs (secrets are write-only)
```

Find missing albums from a script:

```sh
mbid=$(curl -s -H "$DW" "localhost:8080/api/search/artists?q=boards+of+canada" | jq -r '.artists[0].id')
curl -s -H "$DW" "localhost:8080/api/artists/$mbid/releases?missing=only" \
  | jq -r '.releases[] | "\(.firstReleaseDate[:4]) \(.title)"'
```

Push a library without MPD/Subsonic/melody — album entries are either plain titles
or `{"title": ..., "mbid": "<release-group id>"}`:

```sh
curl -s -X POST localhost:8080/api/sync -H "$DW" -H 'Content-Type: application/json' \
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

## License

GNU General Public License v3.0 (GPL-3.0). See [LICENSE](LICENSE).
