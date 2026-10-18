{
  description = "dropwatch: which albums by your artists are you missing?";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.1.2";
      dropwatchFor =
        pkgs:
        pkgs.callPackage ./nix/package.nix {
          inherit version;
          src = self;
          # go.mod requires Go 1.27.
          buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_27 or pkgs.go; };
        };
    in
    {
      packages = forAllSystems (pkgs: rec {
        dropwatch = dropwatchFor pkgs;
        default = dropwatch;
      });

      overlays.default = final: _prev: { dropwatch = dropwatchFor final; };

      nixosModules = rec {
        dropwatch = import ./nix/nixos-module.nix self;
        default = dropwatch;
      };

      homeManagerModules = rec {
        dropwatch = import ./nix/home-manager-module.nix self;
        default = dropwatch;
      };

      checks = forAllSystems (pkgs: {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        nixos = pkgs.testers.runNixOSTest {
          name = "dropwatch";
          nodes.machine = {
            imports = [ self.nixosModules.default ];
            services.dropwatch = {
              enable = true;
              environment.DISCOGS_TOKEN = "seeded-from-env";
            };
          };
          testScript = ''
            machine.wait_for_unit("dropwatch.service")
            machine.wait_for_open_port(8080)
            state = machine.succeed("curl -sf http://127.0.0.1:8080/api/auth")
            assert '"setupRequired":true' in state, state
            machine.succeed("curl -sf -o /tmp/page http://127.0.0.1:8080/ && grep -q '<title>dropwatch' /tmp/page")
            machine.succeed("test -f /var/lib/dropwatch/dropwatch.db")
            machine.succeed("stat -c %U /var/lib/dropwatch/dropwatch.db | grep -qx dropwatch")
            machine.succeed("journalctl -u dropwatch | grep -q 'discogs: true'")
          '';
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
