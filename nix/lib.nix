# Helpers shared by the NixOS and Home Manager modules.
{ lib }:
{
  # -addr value; IPv6 addresses need brackets.
  listenAddress =
    cfg:
    let
      host = if lib.hasInfix ":" cfg.address then "[${cfg.address}]" else cfg.address;
    in
    "${host}:${toString cfg.port}";

  execStart =
    cfg: listen:
    lib.escapeShellArgs (
      [
        (lib.getExe cfg.package)
        "-addr"
        listen
        "-db"
        cfg.database
        "-dump-dir"
        cfg.dumpDir
      ]
      ++ cfg.extraArgs
    );
}
