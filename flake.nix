{
  description = "tether: forwarding manager for remote development";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        tether = pkgs.callPackage ./nix/package.nix {
          version = "0.1.0-${self.shortRev or self.dirtyShortRev or "dev"}";
        };
        default = tether;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            openssh
            gnupg
            sshfs
            gopls
            gotools
            golangci-lint
          ];
        };
      });

      homeManagerModules.default = import ./nix/hm-module.nix self;

      # For the machines you connect to.
      nixosModules.remote = import ./nix/nixos-remote.nix;
      # For the machine you share USB devices from.
      nixosModules.usbip-helper = import ./nix/nixos-usbip-helper.nix self;

      checks = forAllSystems (pkgs: {
        tether = self.packages.${pkgs.stdenv.hostPlatform.system}.tether;
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
