{
  lib,
  buildGoModule,
  makeWrapper,
  gnutar,
  xz,
  src,
  version,
}:

buildGoModule {
  pname = "dropwatch";
  inherit version src;

  vendorHash = "sha256-JIqffDGU076jdWEEW6Uw7c1oC3ZHrr+gEqi6sfdIgAk=";

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=v${version}"
  ];

  nativeBuildInputs = [ makeWrapper ];
  # Dump reading shells out to tar and xz; the tests exercise it.
  nativeCheckInputs = [
    gnutar
    xz
  ];

  postInstall = ''
    wrapProgram $out/bin/dropwatch --prefix PATH : ${
      lib.makeBinPath [
        gnutar
        xz
      ]
    }
  '';

  meta = {
    description = "Tracks the artists in your music library and shows which of their albums you're missing";
    homepage = "https://github.com/carnager/dropwatch";
    license = lib.licenses.gpl3Only;
    mainProgram = "dropwatch";
    platforms = lib.platforms.linux;
  };
}
