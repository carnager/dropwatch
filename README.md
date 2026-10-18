<p align="center">
  <img src="internal/server/web/icon-256.png" alt="dropwatch" width="128">
</p>

<h1 align="center">dropwatch</h1>

<p align="center">
  Which albums by your artists are you missing, and what just dropped?
</p>

<p align="center">
  <a href="https://github.com/carnager/dropwatch/releases/latest">Download</a> ·
  <a href="#nix">Nix</a> ·
  <a href="#api">API</a> ·
  <a href="LICENSE">GPL-3.0</a>
</p>

dropwatch reads your music library from MPD, a Subsonic server (Navidrome,
gonic, Airsonic) or melody, and compares it with each artist's discography on
MusicBrainz, plus Discogs if you like. It lists the albums and EPs you don't
have and tells you when your artists release something new. Remasters, deluxe
editions and reissues count as the album they belong to.

It runs as a single binary with a SQLite database and is configured in the
browser.

## Install

Grab a Linux build from the
[releases](https://github.com/carnager/dropwatch/releases/latest), or build it
yourself with Go 1.27:

```sh
go build -o dropwatch ./cmd/dropwatch
./dropwatch
```

`tar` and `xz` need to be installed; they're used to read the MusicBrainz
dumps.

### Nix

The repository is a flake with a package, a NixOS module and a Home Manager
module:

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
            address = "0.0.0.0";
            openFirewall = true;
          };
        }
      ];
    };
  };
}
```

For a user service, import `dropwatch.homeManagerModules.default` instead. It
has the same options except `openFirewall`, `user` and `group`, and stores its
data in `~/.local/share/dropwatch`.

| Option | Default | |
|---|---|---|
| `address`, `port` | `127.0.0.1`, `8080` | |
| `dataDir` | `/var/lib/dropwatch` | |
| `database` | `<dataDir>/dropwatch.db` | |
| `dumpDir` | `<dataDir>/dumps` | needs ~2.5 GB |
| `environment` | `{}` | ends up in the Nix store |
| `environmentFile` | `null` | for secrets |
| `extraArgs` | `[]` | |
| `openFirewall` | `false` | NixOS only |
| `user`, `group` | `dropwatch` | NixOS only |

The NixOS service is sandboxed and can't read `/home`.

## Setup

Open http://localhost:8080.

1. Choose a password. The UI and the API require it from then on.
2. Under settings → library, enable your sources and test the connection.
   You can use several at once; an album owned in any of them counts. A
   Discogs token is optional.
3. Under settings → musicbrainz, press *check now*. This downloads the
   MusicBrainz release-group dump (about 1 GB) and keeps it.
4. Press *sync library*. Most artists are recognized from the dump within a
   couple of minutes; the rest are looked up online, which is rate-limited
   and takes a few seconds each.

Turn on the schedule in settings → musicbrainz to keep discographies current
and get notified about new releases. MusicBrainz publishes new dumps on
Wednesdays and Saturdays, so Thursday and Sunday mornings work well.

## How it works

dropwatch decides which MusicBrainz artist a library artist is in this order:
a name it already knows, the first artist of an "A & B" credit, the kept dump,
the credited artist of one of your tagged albums, and finally a name search.
The "A & B" and dump steps only accept an artist if one of your albums is in
its discography, and the search only accepts an artist with exactly that name
or alias.

Albums are matched by MusicBrainz ID when your tags have one, otherwise by
title, ignoring case, punctuation and edition suffixes such as "(Deluxe
Edition)". MusicBrainz release groups and Discogs masters are merged by
title, and Discogs-only bootlegs and promos are dropped.

With *only new* checked, a sync makes no requests for artists dropwatch
already knows; their albums are checked against the database. Uncheck it now
and then to refresh everything.

Notifications come from the scheduled dump checks: new albums, EPs and
singles from the last 30 days, upcoming releases, and a reminder on release
day.

Optionally, dropwatch can also process the MusicBrainz release dump (about
21 GB, streamed and not stored). It adds the title and ID of every edition of
the albums you track, so reissue titles and release IDs from Subsonic or melody
match exactly. Turn it on in settings → musicbrainz.

## API

Create a token under settings → account and send it as a bearer token. IDs
are MusicBrainz IDs.

```
GET    /api/search/artists?q=name        search MusicBrainz
GET    /api/artists                      tracked artists with counts
GET    /api/artists/{id}/releases        ?missing=only  ?types=all  ?refresh=1
DELETE /api/artists/{id}                 untrack
GET    /api/library                      everything owned
PUT    /api/releases/{id}/state          {"state": "owned" | "ignored" | ""}
POST   /api/sync                         push a library as JSON
POST   /api/sync/library                 sync enabled sources  ?artist=  ?exact=1  ?skip_existing=1
GET    /api/sync/status
GET    /api/notifications                ?types=Album,EP,Single
POST   /api/notifications/read           all (same ?types=)
POST   /api/notifications/{id}/read
GET    /api/dumps                        schedules and download status
PUT    /api/dumps/schedule               {"enabled":true,"days":[0,4],"time":"04:00","timezone":"Europe/Berlin"}
POST   /api/dumps/check
PUT    /api/dumps/releases               {"enabled":true,"intervalDays":30}
POST   /api/dumps/releases/check
GET    /api/settings                     PUT to change; secrets are write-only
```

```sh
auth="Authorization: Bearer $TOKEN"
mbid=$(curl -s -H "$auth" "localhost:8080/api/search/artists?q=boards+of+canada" | jq -r '.artists[0].id')
curl -s -H "$auth" "localhost:8080/api/artists/$mbid/releases?missing=only" \
  | jq -r '.releases[] | "\(.firstReleaseDate[:4]) \(.title)"'
```

`POST /api/sync` takes `{"artists": [{"name": "...", "albums": ["...", ...]}]}`;
an album can also be `{"title": "...", "mbid": "<release group id>"}`.

## Flags

`-addr` (default `:8080`), `-db` (default `dropwatch.db`) and `-dump-dir`
(default `<db>.dumps`). Everything else is configured in the web UI.

`-mpd`, `-subsonic`, `-melody` and `-discogs-token`, or the environment
variables `MPD_HOST`, `MPD_PASSWORD`, `SUBSONIC_URL`, `SUBSONIC_USER`,
`SUBSONIC_PASSWORD`, `MELODY_SERVER`, `MELODY_PASSWORD`,
`MELODY_PASSWORD_FILE` and `DISCOGS_TOKEN`, prefill the settings when
dropwatch creates a new database. They are ignored after that.

`dropwatch import` imports a library from dump files on the command line.

## License

GPL-3.0, see [LICENSE](LICENSE).
