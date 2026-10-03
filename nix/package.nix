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

  # Static, so `tether usbip-helper install` can copy it out of the store.
  env.CGO_ENABLED = 0;

  vendorHash = "sha256-yXtS4oT6I/uhlEAViZlTHvK5YWeeG7J1gaLA6imbF3I=";

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
