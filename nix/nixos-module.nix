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
  inherit (lib) mkIf mkOption types;
in
{
  options.services.dropwatch =
    import ./options.nix self {
      inherit lib pkgs cfg;
      defaultDataDir = "/var/lib/dropwatch";
      defaultDataDirText = ''"/var/lib/dropwatch"'';
    }
    // {
      openFirewall = mkOption {
        type = types.bool;
        default = false;
        description = "Open {option}`services.dropwatch.port` in the firewall.";
      };

      user = mkOption {
        type = types.str;
        default = "dropwatch";
        description = "User account dropwatch runs as. Created if left at the default.";
      };

      group = mkOption {
        type = types.str;
        default = "dropwatch";
        description = "Group dropwatch runs as. Created if left at the default.";
      };
    };

  config = mkIf cfg.enable {
    users.users = mkIf (cfg.user == "dropwatch") {
      dropwatch = {
        isSystemUser = true;
        inherit (cfg) group;
        home = cfg.dataDir;
      };
    };
    users.groups = mkIf (cfg.group == "dropwatch") { dropwatch = { }; };

    systemd.tmpfiles.settings."10-dropwatch" =
      lib.genAttrs
        (lib.unique [
          cfg.dataDir
          cfg.dumpDir
          (dirOf cfg.database)
        ])
        (_: {
          d = {
            inherit (cfg) user group;
            mode = "0750";
          };
        });

    systemd.services.dropwatch = {
      description = "dropwatch";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      inherit (cfg) environment;

      serviceConfig = {
        ExecStart = helpers.execStart cfg (helpers.listenAddress cfg);
        EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
        User = cfg.user;
        Group = cfg.group;
        WorkingDirectory = cfg.dataDir;
        Restart = "on-failure";
        RestartSec = 5;

        ReadWritePaths = lib.unique [
          cfg.dataDir
          cfg.dumpDir
          (dirOf cfg.database)
        ];
        CapabilityBoundingSet = "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        PrivateUsers = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        UMask = "0027";
      };
    };

    networking.firewall.allowedTCPPorts = mkIf cfg.openFirewall [ cfg.port ];
  };
}
