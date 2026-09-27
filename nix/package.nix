{
  lib,
  buildGoModule,
  version ? "dev",
}:

buildGoModule {
  pname = "tether";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  vendorHash = "sha256-n58Qmiv3gik1qkuXQFbQ+soeOQtUz1dUocEAJepqp/E=";

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  meta = {
    description = "Forwarding manager for remote development: port forwards, gpg-agent forwarding and mounts over SSH";
    mainProgram = "tether";
    platforms = lib.platforms.linux;
  };
}
