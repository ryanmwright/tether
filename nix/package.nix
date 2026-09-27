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

  vendorHash = "sha256-/xSrmHfgou24I2Hs5JO/eQ3mpptdNMSF+NlfIy75HVw=";

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
