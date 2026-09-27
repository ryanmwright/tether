self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.programs.tether;
  tomlFormat = pkgs.formats.toml { };
in
{
  options.programs.tether = {
    enable = lib.mkEnableOption "tether, a forwarding manager for remote development";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "tether.packages.\${system}.default";
      description = "The tether package to use.";
    };

    settings = lib.mkOption {
      type = lib.types.nullOr tomlFormat.type;
      default = null;
      description = ''
        Contents of {file}`$XDG_CONFIG_HOME/tether/config.toml`. When `null`
        (the default) the file is not managed, so it can be edited by hand.
      '';
      example = lib.literalExpression ''
        {
          hosts.devbox = { ssh = "devbox"; autoconnect = true; };
          profiles.work = {
            host = "devbox";
            gpg = true;
            forwards = [ "L:5432:db.internal:5432" "R:8080:localhost:3000" ];
          };
        }
      '';
    };

    tray = {
      enable = lib.mkEnableOption "the tether tray icon (StatusNotifierItem), started with the graphical session";

      extraArgs = lib.mkOption {
        type = with lib.types; listOf str;
        default = [ ];
        example = [
          "--terminal"
          "konsole"
          "--terminal"
          "-e"
        ];
        description = "Extra arguments for `tether tray`, e.g. the terminal to open the TUI in.";
      };
    };

    service = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Run the daemon as a systemd user service started at login.";
      };

      path = lib.mkOption {
        type = with lib.types; listOf str;
        default = [
          "${config.home.profileDirectory}/bin"
          "/run/wrappers/bin"
          "/usr/local/bin"
          "/usr/bin"
          "/bin"
        ];
        description = ''
          PATH for the daemon, used to find ssh, sshfs, gpgconf and fusermount3.
          System directories are included so that, on non-NixOS hosts, the
          system OpenSSH and the setuid fusermount3 are found.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = !cfg.tray.enable || cfg.service.enable;
        message = "programs.tether.tray needs programs.tether.service.enable (the tray talks to the daemon).";
      }
    ];

    home.packages = [ cfg.package ];

    xdg.configFile."tether/config.toml" = lib.mkIf (cfg.settings != null) {
      source = tomlFormat.generate "tether-config.toml" cfg.settings;
      onChange = "${lib.getExe cfg.package} config reload --no-autostart >/dev/null 2>&1 || true";
    };

    systemd.user.services.tether = lib.mkIf cfg.service.enable {
      Unit.Description = "tether forwarding manager";
      Service = {
        Type = "notify";
        ExecStart = "${lib.getExe cfg.package} daemon";
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        RestartSec = 2;
        Environment = [ "PATH=${lib.concatStringsSep ":" cfg.service.path}" ];
      };
      Install.WantedBy = [ "default.target" ];
    };

    systemd.user.services.tether-tray = lib.mkIf cfg.tray.enable {
      Unit = {
        Description = "tether tray icon";
        PartOf = [ "graphical-session.target" ];
        After = [
          "graphical-session.target"
          "tray.target"
          "tether.service"
        ];
        Wants = [ "tether.service" ];
      };
      Service = {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "tray"
          ]
          ++ cfg.tray.extraArgs
        );
        Restart = "on-failure";
        RestartSec = 2;
        Environment = [ "PATH=${lib.concatStringsSep ":" cfg.service.path}" ];
      };
      Install.WantedBy = [ "graphical-session.target" ];
    };
  };
}
