# Options shared by the NixOS and Home Manager modules.
self:
{
  lib,
  pkgs,
  cfg,
  defaultDataDir,
  defaultDataDirText,
}:
let
  inherit (lib) mkOption mkEnableOption types;
in
{
  enable = mkEnableOption "dropwatch, which tracks the artists in your music library and shows which of their albums you're missing";

  package = mkOption {
    type = types.package;
    default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
    defaultText = lib.literalExpression "dropwatch.packages.\${pkgs.stdenv.hostPlatform.system}.default";
    description = "The dropwatch package to run.";
  };

  address = mkOption {
    type = types.str;
    default = "127.0.0.1";
    example = "0.0.0.0";
    description = ''
      Address the web UI and API listen on. The default only accepts
      connections from this machine; use `0.0.0.0` (or `::`) for all
      interfaces. The UI is password protected either way.
    '';
  };

  port = mkOption {
    type = types.port;
    default = 8080;
    description = "Port the web UI and API listen on.";
  };

  dataDir = mkOption {
    type = types.str;
    default = defaultDataDir;
    defaultText = lib.literalExpression defaultDataDirText;
    description = "Directory holding the database and the MusicBrainz dump.";
  };

  database = mkOption {
    type = types.str;
    default = "${cfg.dataDir}/dropwatch.db";
    defaultText = lib.literalExpression ''"''${config.services.dropwatch.dataDir}/dropwatch.db"'';
    description = "Path of the SQLite database (`-db`).";
  };

  dumpDir = mkOption {
    type = types.str;
    default = "${cfg.dataDir}/dumps";
    defaultText = lib.literalExpression ''"''${config.services.dropwatch.dataDir}/dumps"'';
    description = ''
      Where the MusicBrainz release-group dump is kept (`-dump-dir`). Needs
      room for two compressed dumps, about 2.5 GB.
    '';
  };

  environment = mkOption {
    type = types.attrsOf types.str;
    default = { };
    example = {
      MPD_HOST = "localhost:6600";
      MELODY_SERVER = "music.local:6603";
    };
    description = ''
      Environment variables for dropwatch. Library sources and the Discogs
      token are configured in the web UI; the legacy variables (`MPD_HOST`,
      `MPD_PASSWORD`, `SUBSONIC_URL`, `SUBSONIC_USER`, `SUBSONIC_PASSWORD`,
      `MELODY_SERVER`, `MELODY_PASSWORD`, `MELODY_PASSWORD_FILE`,
      `DISCOGS_TOKEN`) only fill in those settings when the database is
      first created. Values end up in the Nix store; put secrets in
      {option}`environmentFile` instead.
    '';
  };

  environmentFile = mkOption {
    type = types.nullOr types.path;
    default = null;
    example = "/run/secrets/dropwatch.env";
    description = ''
      File with `NAME=value` lines loaded into dropwatch's environment, for
      secrets such as `DISCOGS_TOKEN` or `MELODY_PASSWORD`. Read by systemd,
      so it never enters the Nix store.
    '';
  };

  extraArgs = mkOption {
    type = types.listOf types.str;
    default = [ ];
    description = "Additional command-line arguments.";
  };
}
