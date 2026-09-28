# The privileged USB/IP helper, for the machine you share USB devices from.
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.tether-usbip;
in
{
  options.services.tether-usbip = {
    enable = lib.mkEnableOption "the tether USB/IP helper, which shares this machine's USB devices with remotes";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "tether.packages.\${system}.default";
      description = "The tether package to use.";
    };

    users = lib.mkOption {
      type = with lib.types; listOf str;
      example = [ "alice" ];
      description = "Users whose tether daemon may share devices.";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:3240";
      description = "USB/IP address, on IPv4 loopback. Change the port if usbipd already uses 3240.";
    };
  };

  config = lib.mkIf cfg.enable {
    boot.kernelModules = [ "usbip-host" ];

    systemd.services.tether-usbip = {
      description = "tether USB/IP helper";
      wantedBy = [ "multi-user.target" ];
      path = [ pkgs.kmod ];
      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "usbip-helper"
            "--listen"
            cfg.listen
          ]
          ++ lib.concatMap (u: [
            "--allow-user"
            u
          ]) cfg.users
        );
        Restart = "on-failure";
        RuntimeDirectory = "tether-usbip";
        RuntimeDirectoryMode = "0755";
      };
    };
  };
}
