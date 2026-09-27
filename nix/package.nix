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

  vendorHash = "sha256-Q8mJjiWVoVkwAS7WkNLUHQ63X4SRKEYHSXzAAgJ5lL0=";

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
