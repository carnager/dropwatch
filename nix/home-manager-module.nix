self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.dropwatch;
  helpers = import ./lib.nix { inherit lib; };
  # systemd unit syntax: double quotes keep values with spaces in one piece.
  quote = s: ''"${lib.replaceStrings [ "\\" "\"" ] [ "\\\\" "\\\"" ] s}"'';
in
{
  options.services.dropwatch = import ./options.nix self {
    inherit lib pkgs cfg;
    defaultDataDir = "${config.xdg.dataHome}/dropwatch";
    defaultDataDirText = ''"''${config.xdg.dataHome}/dropwatch"'';
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    systemd.user.services.dropwatch = {
      Unit = {
        Description = "dropwatch";
        After = [ "network-online.target" ];
        Wants = [ "network-online.target" ];
      };
      Service = {
        ExecStartPre = lib.escapeShellArgs (
          [
            "${pkgs.coreutils}/bin/mkdir"
            "-p"
          ]
          ++ lib.unique [
            cfg.dataDir
            cfg.dumpDir
            (dirOf cfg.database)
          ]
        );
        ExecStart = helpers.execStart cfg (helpers.listenAddress cfg);
        Environment = lib.mapAttrsToList (name: value: quote "${name}=${value}") cfg.environment;
        EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
        Restart = "on-failure";
        RestartSec = 5;
      };
      Install.WantedBy = [ "default.target" ];
    };
  };
}
