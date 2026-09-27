# Remote-side setup for machines you connect to with tether.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.tether.remote;
in
{
  options.tether.remote = {
    enable = lib.mkEnableOption "setup for machines that tether connects to";

    mounts.enable = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Let tether mount your local directories on this machine: installs
        sshfs and enables FUSE (the setuid fusermount wrappers).
      '';
    };

    gpg = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Prepare this machine to receive a forwarded gpg-agent: install
          GnuPG, and let sshd replace stale forwarded sockets
          (`StreamLocalBindUnlink`). This machine must not run a gpg-agent of
          its own, so `programs.gnupg.agent.enable` must stay off.
        '';
      };

      sshAuthSock = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          In SSH sessions, point `SSH_AUTH_SOCK` at the gpg-agent SSH socket,
          where tether forwards your local agent when a profile sets
          `gpg_ssh = true`.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      (lib.mkIf cfg.mounts.enable {
        environment.systemPackages = [ pkgs.sshfs ];
        programs.fuse.enable = true;
      })

      (lib.mkIf cfg.gpg.enable {
        assertions = [
          {
            assertion = !config.programs.gnupg.agent.enable;
            message = ''
              tether.remote.gpg: programs.gnupg.agent.enable starts a gpg-agent on this
              machine that competes with the one tether forwards. Disable it, or set
              tether.remote.gpg.enable = false.
            '';
          }
        ];

        environment.systemPackages = [ pkgs.gnupg ];

        services.openssh.settings.StreamLocalBindUnlink = true;

        environment.extraInit = lib.mkIf cfg.gpg.sshAuthSock ''
          if [ -n "$SSH_CONNECTION" ]; then
            export SSH_AUTH_SOCK="$(${pkgs.gnupg}/bin/gpgconf --list-dirs agent-ssh-socket)"
          fi
        '';
      })
    ]
  );
}
