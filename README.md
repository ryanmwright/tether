# tether

A forwarding manager for remote development. A small background daemon keeps
your SSH connections to dev machines up and manages what flows over them:

- **Port forwards** in both directions (`-L`, `-R`, SOCKS `-D`), including
  reaching other hosts through a bastion
- **gpg-agent forwarding** for commit signing, `pass`, and gpg-agent as your
  SSH agent on the remote
- **Directory mounts** in both directions: a remote directory here, or a
  local directory on the remote (sshfs over the same connection)
- Later: USB forwarding (USB/IP)

You drive it from the CLI, a terminal UI, or a system tray icon. It uses your
system OpenSSH, so everything in `~/.ssh/config` (`ProxyJump`, keys, FIDO
tokens, `known_hosts`) works as it already does.

> **Status: early development.** Connections, port forwards (`L`/`R`/`D`,
> TCP and Unix sockets), automatic reconnects, gpg-agent forwarding, directory
> mounts, profiles, `tether doctor`, the CLI, the terminal UI and the tray icon
> work today. See [PLAN.md](PLAN.md) for the roadmap.

## Contents

- [How it runs](#how-it-runs)
- [Installation](#installation)
  - [Nix + home-manager (NixOS or any other distro)](#nix--home-manager-nixos-or-any-other-distro)
  - [NixOS without home-manager](#nixos-without-home-manager)
  - [Nix on another distro, without home-manager](#nix-on-another-distro-without-home-manager)
  - [Without Nix](#without-nix)
- [Configuration file](#configuration-file)
- [gpg-agent forwarding](#gpg-agent-forwarding)
- [Directory mounts](#directory-mounts)
- [Terminal UI](#terminal-ui)
- [Tray icon](#tray-icon)
- [CLI](#cli)
- [Paths and environment variables](#paths-and-environment-variables)
- [Development](#development)

## How it runs

`tether` is a single binary. `tether daemon` runs the background service; every
other subcommand talks to it over a Unix socket
(`$XDG_RUNTIME_DIR/tether/tether.sock`, readable only by you).

The daemon can be started in two ways:

1. **As a systemd user service** (recommended). It starts at login, so hosts
   marked `autoconnect` come up without you running anything.
2. **On demand.** If the daemon isn't running, any `tether` command starts it.
   If a `tether.service` user unit is installed and you're using the default
   socket and config paths, it runs `systemctl --user start tether.service`.
   Otherwise it starts `tether daemon` in the background, logging to
   `$XDG_RUNTIME_DIR/tether/daemon.log`. Pass `--no-autostart` to turn this off.

Only one daemon runs per socket; a second one refuses to start.

**After upgrading**, an older daemon may still be running. Commands then say so
and stop; restart it with `systemctl --user restart tether`, or
`tether daemon stop` if it was started on demand (the next command starts the
new one). Restarting drops and re-establishes your connections.

### Connections

The daemon keeps one OpenSSH connection (a `ControlMaster`) per host and adds
or removes forwards on it while it runs. It reconnects on its own after drops,
with a backoff between attempts, and right away when your network changes
(new Wi-Fi, VPN up, resume from suspend). Forwards come back with it.

Because the daemon runs in the background, ssh can't prompt you. Before
tether can connect to a host:

- **The host key must already be known.** Run `ssh <host>` once and accept
  it.
- **Authentication must work without typing anything**: a key loaded in your
  agent (ssh-agent or gpg-agent), or a hardware key that only needs a touch.
  If the daemon has no `SSH_AUTH_SOCK` (common for systemd user services), it
  uses gpg-agent's SSH socket when one exists.

Forwards listed in `~/.ssh/config` (`LocalForward` and friends) are ignored on
tether's connections; tether only opens the forwards you ask it for.

## Installation

Requirements on the local machine: Linux, OpenSSH, and (for the features that
use them) `gpg` and `sshfs`/FUSE.

### Nix + home-manager (NixOS or any other distro)

This is the most complete setup. The home-manager module installs the package,
adds a `tether.service` systemd user unit started at login, and can optionally
write the config file for you. It works the same whether home-manager runs
standalone (e.g. on Fedora) or as a NixOS module.

Add the flake input and import the module:

```nix
# flake.nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    home-manager.url = "github:nix-community/home-manager";
    home-manager.inputs.nixpkgs.follows = "nixpkgs";
    tether.url = "github:ryanmwright/tether";
    tether.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { nixpkgs, home-manager, tether, ... }: {
    # Standalone home-manager (e.g. Fedora + Nix):
    homeConfigurations."you" = home-manager.lib.homeManagerConfiguration {
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
      modules = [
        tether.homeManagerModules.default
        ./home.nix
      ];
    };

    # Or, home-manager as a NixOS module:
    # home-manager.users.you.imports = [ tether.homeManagerModules.default ];
  };
}
```

Then enable it. There are two ways to handle the config file.

**Option A: manage the config in Nix.** Set `settings` and the module writes
`~/.config/tether/config.toml` from it. On each `home-manager switch` the
running daemon is told to reload.

```nix
# home.nix
{
  programs.tether = {
    enable = true;
    settings = {
      defaults.reconnect_backoff = "1s..60s";

      hosts.devbox = {
        ssh = "devbox";          # alias from ~/.ssh/config
        autoconnect = true;
      };

      profiles.work = {
        host = "devbox";
        gpg = true;               # forward gpg-agent
        gpg_ssh = true;           # ...and its SSH socket
        forwards = [
          "L:5432:db.internal:5432"
          "R:8080:localhost:3000"
          "D:1080"
        ];
        mounts = [
          { direction = "remote-to-local"; remote = "~/src"; local = "~/mnt/devbox/src"; }
        ];
      };
    };
  };
}
```

**Option B: edit the config by hand.** Leave `settings` unset (the default).
The module installs the package and service but never touches
`~/.config/tether/config.toml`, so you can edit it yourself and run
`tether config reload`.

```nix
{ programs.tether.enable = true; }
```

All module options:

| Option | Default | Description |
|---|---|---|
| `programs.tether.enable` | `false` | Install tether and (by default) its user service. |
| `programs.tether.package` | the flake's package | Package to use. |
| `programs.tether.settings` | `null` | Config file contents as a Nix attrset. `null` means the file is not managed. |
| `programs.tether.service.enable` | `true` | Install `tether.service` and start it at login (`default.target`). |
| `programs.tether.tray.enable` | `false` | Start the [tray icon](#tray-icon) with the graphical session. |
| `programs.tether.tray.extraArgs` | `[]` | Extra `tether tray` arguments, e.g. `--terminal`. |
| `programs.tether.service.path` | see below | `PATH` for the daemon, used to find `ssh`, `sshfs`, `gpgconf` and `fusermount3`. |

`service.path` defaults to
`~/.nix-profile/bin:/run/wrappers/bin:/usr/local/bin:/usr/bin:/bin` (the first
entry follows your home-manager profile directory).

#### Notes for non-NixOS distros (Fedora, Debian, ...)

- **Use the system's OpenSSH and FUSE helpers.** A Nix-built `sshfs` still
  needs the distro's setuid `/usr/bin/fusermount3`, so install `fuse3` (and
  `fuse-sshfs` on Fedora, `sshfs` on Debian) from your package manager. The
  default `service.path` already includes `/usr/bin`. If you'd rather use your
  distro's `ssh` than a Nix one, reorder `service.path` so `/usr/bin` comes
  first.
- **gpg from Nix or from the distro both work**, as long as the `gpgconf` on
  the daemon's `PATH` belongs to the same GnuPG install that runs your agent.
- home-manager user services need a systemd user session, which desktop
  logins on Fedora and Debian have. Check with `systemctl --user status`.

### NixOS without home-manager

Install the package system-wide and declare the user service yourself. This
assumes the flake input is passed to your modules, e.g.
`nixpkgs.lib.nixosSystem { specialArgs = { inherit tether; }; ... }`.

```nix
{ pkgs, tether, ... }:
let
  tetherPkg = tether.packages.${pkgs.stdenv.hostPlatform.system}.default;
in
{
  environment.systemPackages = [ tetherPkg ];

  systemd.user.services.tether = {
    description = "tether forwarding manager";
    wantedBy = [ "default.target" ];
    path = [ pkgs.openssh pkgs.sshfs pkgs.gnupg "/run/wrappers" ];
    serviceConfig = {
      Type = "notify";
      ExecStart = "${tetherPkg}/bin/tether daemon";
      ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
      Restart = "on-failure";
      RestartSec = 2;
    };
  };
}
```

Then write `~/.config/tether/config.toml` by hand (see
[Configuration file](#configuration-file)).

### Nix on another distro, without home-manager

Install into your profile:

```sh
nix profile install github:ryanmwright/tether
# or try it without installing:
nix run github:ryanmwright/tether -- status
```

That's enough to use tether; the first command you run starts the daemon on
demand. To start it at login instead, add the user unit from
[Without Nix](#without-nix) and set `ExecStart` to
`%h/.nix-profile/bin/tether daemon`.

### Without Nix

Install with Go 1.26 or newer:

```sh
GOBIN=~/.local/bin go install github.com/ryanmwright/tether/cmd/tether@latest
```

or build from a checkout:

```sh
git clone https://github.com/ryanmwright/tether && cd tether
go build -o ~/.local/bin/tether ./cmd/tether
```

Install the runtime tools from your package manager:

```sh
# Fedora
sudo dnf install openssh-clients gnupg2 fuse3 fuse-sshfs
# Debian / Ubuntu
sudo apt install openssh-client gnupg fuse3 sshfs
```

Optionally, run the daemon at login with a systemd user unit. The unit must be
named `tether.service` for on-demand startup to use it.

```ini
# ~/.config/systemd/user/tether.service
[Unit]
Description=tether forwarding manager

[Service]
Type=notify
ExecStart=%h/.local/bin/tether daemon
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now tether.service
```

Without the unit, tether still works: the first command you run starts the
daemon in the background.

## Configuration file

Default location: `~/.config/tether/config.toml` (`$XDG_CONFIG_HOME/tether/config.toml`).
A missing file is fine and means "nothing configured".

```toml
[defaults]
reconnect_backoff = "1s..60s"

[hosts.devbox]
ssh = "devbox"                 # destination passed to ssh; usually a ~/.ssh/config alias
autoconnect = true

[hosts.prod-bastion]           # `ssh` defaults to the host's name

[profiles.work]
host = "devbox"
autoconnect = false
gpg = true
forwards = [
  "L:5432:db.internal:5432",   # local 5432 -> db.internal:5432 as seen from devbox
  "R:8080:localhost:3000",     # devbox's 8080 -> your local 3000
  "D:1080",                    # SOCKS proxy on local 1080
]

[[profiles.work.mounts]]
direction = "remote-to-local"
remote = "~/src"
local = "~/mnt/devbox/src"
options = ["reconnect"]
```

### Reference

**`[defaults]`**

| Key | Type | Default | Description |
|---|---|---|---|
| `reconnect_backoff` | string | `"1s..60s"` | Reconnect delay range `"min..max"`, or a single duration for a fixed delay. Go duration syntax (`500ms`, `10s`, `2m`). |

**`[hosts.<name>]`**: a machine you connect to. Names may contain letters,
digits, `.`, `_` and `-`.

| Key | Type | Default | Description |
|---|---|---|---|
| `ssh` | string | the host's name | Destination passed to `ssh`: an alias from `~/.ssh/config`, or `user@host`. Put bastions, ports, users and keys in `~/.ssh/config` (e.g. `ProxyJump`). |
| `autoconnect` | bool | `false` | Connect when the daemon starts and after network changes. |

**`[profiles.<name>]`**: a named bundle of forwards, gpg and mounts on one
host. Several profiles on the same host share one SSH connection.

| Key | Type | Default | Description |
|---|---|---|---|
| `host` | string | required | Name of a `[hosts.*]` entry. |
| `autoconnect` | bool | `false` | Bring this profile up when the daemon starts. |
| `gpg` | bool | `false` | Forward your gpg-agent, so gpg on the host signs and decrypts with your local keys. See [gpg-agent forwarding](#gpg-agent-forwarding). |
| `gpg_ssh` | bool | `false` | Also forward gpg-agent's SSH socket, so ssh on the host can use your SSH keys. |
| `forwards` | list of strings | `[]` | Port forwards; see below. |
| `mounts` | list of tables | `[]` | Directory mounts; see below. |

**Forward specs** (`forwards`):

| Spec | Meaning |
|---|---|
| `L:[bind:]port:host:hostport` | Local forward: a local port reaches `host:hostport` from the remote side. `host` can be any machine the remote can reach, which is how you use a host as a bastion. |
| `R:[bind:]port:host:hostport` | Remote forward: a port on the remote reaches `host:hostport` from your side. |
| `D:[bind:]port` | Dynamic (SOCKS) proxy on a local port. |

**`[[profiles.<name>.mounts]]`**:

| Key | Type | Description |
|---|---|---|
| `direction` | `"remote-to-local"` or `"local-to-remote"` | Remote-to-local mounts a remote directory on your machine. Local-to-remote mounts one of your directories on the remote. See [Directory mounts](#directory-mounts). |
| `remote` | string | Path on the remote. Relative paths and `~` are from the remote home. |
| `local` | string | Path on your machine: absolute, or starting with `~/`. |
| `options` | list of strings | Extra `sshfs -o` options, e.g. `["ro"]`. |

Unknown keys are errors (so typos are caught), and every problem is reported
at once. Check a file without touching the daemon:

```sh
tether config check
```

After editing, apply it with `tether config reload` (or
`systemctl --user reload tether`). If the new file is invalid, the daemon keeps
the previous config and `tether status` shows the error.

### What reloading changes

Reloading applies changes to running connections without dropping them:
forwards added to an active profile are opened, removed ones are closed.
Hosts or profiles removed from the file are taken down. Changing a host's
`ssh` destination reconnects it. `autoconnect` applies when the daemon starts,
and on reload to hosts and profiles that are new or newly marked
`autoconnect`; it won't bring back something you took down by hand.

### Remote machines

Remotes need only `sshd`. gpg forwarding needs a little remote setup (below),
and local-to-remote mounts need `sshfs` and FUSE there. Run
`tether doctor HOST` to check a host.

## gpg-agent forwarding

With `gpg = true` in a profile (or `tether gpg on HOST`), gpg on the remote
uses your **local** gpg-agent: commit signing, `pass`, decrypting files. Your
secret keys never leave your machine, and passphrase prompts (pinentry)
appear locally.

tether forwards gpg-agent's *extra* socket, which gpg-agent restricts for
exactly this use: remote callers can sign and decrypt, but can't export your
secret keys or shut the agent down.

With `gpg_ssh = true` (or `tether gpg on HOST --ssh`), gpg-agent's SSH
socket is forwarded too, so ssh on the remote (e.g. `git push` over SSH) can
use the SSH keys held by your gpg-agent. That socket isn't restricted: anyone
with root on the remote can use your SSH keys while it's forwarded, so only
turn it on for machines you trust.

Each time the connection comes up, tether asks both ends' `gpgconf` where
the sockets live, stops the remote's own gpg-agent if one is running, and
forwards your local sockets to the remote's standard paths. `tether status`
shows each gpg forward and the remote socket it's bound to.

### Your local gpg-agent

tether forwards gpg-agent's *extra* socket (`gpgconf --list-dirs
agent-extra-socket`), and for `gpg_ssh` its SSH socket. gpg-agent creates the
extra socket by default, **except** when systemd starts it through socket
units: then it only has the sockets systemd hands it. With home-manager's
`services.gpg-agent`, turn it on:

```nix
services.gpg-agent = {
  enable = true;
  enableExtraSocket = true;   # needed for tether's gpg forwarding
  enableSshSupport = true;    # for gpg_ssh
};
```

Without home-manager, enable `gpg-agent-extra.socket` (and
`gpg-agent-ssh.socket`) with `systemctl --user enable --now`. `tether doctor`
reports a missing socket.

### Setting up a remote

1. **Install GnuPG** on the remote.
2. **Import your public keys** there. gpg needs them to know which keys the
   agent holds; `tether doctor` prints the exact command, which looks like:
   `gpg --export YOURKEYID | ssh devbox gpg --import`
3. **Don't run a gpg-agent on the remote.** tether stops a running one when
   it connects, but a socket-activated agent (systemd user units) can come
   back. Mask its units:
   `systemctl --user mask --now gpg-agent.socket gpg-agent-extra.socket gpg-agent-ssh.socket gpg-agent-browser.socket gpg-agent.service`
4. **Recommended:** set `StreamLocalBindUnlink yes` in the remote's
   `/etc/ssh/sshd_config` (then reload sshd). tether clears stale sockets
   itself, but this also covers sshd holding on to a socket from a connection
   that died uncleanly.
5. **For `gpg_ssh`:** point `SSH_AUTH_SOCK` at the forwarded socket in your
   remote shell's startup file:
   `export SSH_AUTH_SOCK="$(gpgconf --list-dirs agent-ssh-socket)"`
   If you set `GNUPGHOME` on the remote, do this after it: the socket path
   depends on it.

Then check it:

```console
$ tether doctor devbox
local
  ok    ssh               OpenSSH_10.5p1, OpenSSL 3.6.4 25 Aug 2026
  ok    ssh agent         /run/user/1000/gnupg/S.gpg-agent.ssh (1 keys)
  ok    gpg-agent         extra socket /run/user/1000/gnupg/S.gpg-agent.extra

connection
  ok    connect           connected to devbox

remote
  ok    gpg               gpg (GnuPG) 2.4.7
  ok    agent socket      /run/user/1000/gnupg/S.gpg-agent
  WARN  remote gpg-agent  these units can start a gpg-agent on devbox that takes over the forwarded socket: gpg-agent.socket
                          fix: ssh devbox systemctl --user mask --now gpg-agent.socket ...
  ok    public keys       all 1 local keys are known on devbox
```

`doctor` only inspects; it never changes anything. If the host isn't
connected, it connects briefly for the checks. It exits non-zero if any check
fails.

### NixOS remotes

The flake has a module for the machines you connect to. It installs GnuPG,
sets `StreamLocalBindUnlink yes`, sets `SSH_AUTH_SOCK` in SSH sessions for
`gpg_ssh`, and fails the build if `programs.gnupg.agent.enable` is on (that
starts a competing agent). It also installs `sshfs` and enables FUSE, for
mounting your local directories there:

```nix
{
  imports = [ tether.nixosModules.remote ];
  tether.remote.enable = true;
  # tether.remote.gpg.enable = true;       # default
  # tether.remote.gpg.sshAuthSock = true;  # default
  # tether.remote.mounts.enable = true;    # default: sshfs + FUSE for local-to-remote mounts
}
```

You still need to import your public keys (step 2). If you manage the
remote user with home-manager, don't enable `services.gpg-agent` there.

## Directory mounts

Mounts go over the host's existing connection. Mark the remote side with
`remote:`, like scp:

```console
$ tether mount add devbox remote:~/src ~/mnt/src     # the remote's ~/src, here
remote:~/src -> /home/you/mnt/src: up
$ tether mount add devbox ~/proj remote:~/proj       # your ~/proj, on the remote
/home/you/proj -> remote:~/proj: up
$ tether mount rm devbox remote:~/src ~/mnt/src
```

Or permanently, in a profile:

```toml
[[profiles.work.mounts]]
direction = "remote-to-local"
remote = "~/src"
local = "~/mnt/src"

[[profiles.work.mounts]]
direction = "local-to-remote"
local = "~/proj"
remote = "~/proj"
options = ["ro"]          # extra sshfs -o options
```

- **Remote directory here** (`remote-to-local`): runs `sshfs` on this machine,
  talking to the remote's SFTP server. Needs `sshfs` and FUSE here (Fedora:
  `fuse-sshfs`; Debian: `sshfs`). The mount point is created if missing.
- **Local directory on the remote** (`local-to-remote`): runs `sshfs` on the
  remote, served by an SFTP server inside tether. That server is confined to
  the directory you share: the remote can't reach anything else on your
  machine, not through `..` and not through symlinks. (The usual "reverse
  sshfs" trick uses OpenSSH's `sftp-server`, which would expose everything
  your user can read.) Needs `sshfs` and FUSE on the remote. The local
  directory must exist; the remote mount point is created.

File ownership is mapped to your user on each side (`idmap=user`) unless you
pass another `idmap` option.

Mounts come and go with the connection. When the connection drops, they're
unmounted, and they come back when it reconnects. Programs using them see
errors in between. If a mount goes away on its own (unmounted by hand,
`sshfs` crashed), tether notices and mounts it again. Taking a host or
profile down unmounts cleanly, lazily if something is still using it.

`tether doctor HOST` checks for `sshfs`, a setuid `fusermount` and
`/dev/fuse` on whichever side needs them.

On a non-NixOS machine where Nix provides your tools, install `sshfs` and
`fuse3` from the distribution anyway: FUSE mounts need the distribution's
setuid `fusermount3`, which a Nix-built one isn't.

## Terminal UI

`tether tui` shows everything live and lets you change it with single keys:

```
tether  0.1.0 · pid 81234 · up 2h13m · /home/you/.config/tether/config.toml

HOSTS
  ● devbox                    up        ssh devbox · auto
  ●   D:1080                  up        ad-hoc
  ●   L:5432:db.internal:5432 up        work
  ●   gpg-agent               up        work  R:/run/user/1000/gnupg/S.gpg-agent:…
▸ ✕ lab                       error     ssh lab  Permission denied (publickey) (retry in 8s)

PROFILES
  ● work                      up        on devbox
lab: Permission denied (publickey) (retry in 8s)
LOG
14:01:53 INFO connected host=devbox
14:01:55 WARN connect failed host=lab err="Permission denied (publickey)"
enter toggle · a add forward · g gpg · d doctor · r reload · l log · ? help · q quit
```

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Move |
| `enter`, `space` | Toggle: connect/disconnect a host, activate/deactivate a profile, remove an ad-hoc forward or mount. The footer says which |
| `u` | Bring the selection up now; on a failed host, retry without waiting |
| `x` | Take the selection down; on an ad-hoc host that's already down, forget it |
| `c` | Connect to a host that isn't in the config: `NAME [SSH-DEST]` |
| `a` | Add an ad-hoc forward to the selected host (any spec, or `gpg-agent`/`gpg-ssh`) |
| `m` | Add an ad-hoc mount on the selected host: `SRC DST`, one side `remote:PATH` |
| `g` | Toggle ad-hoc gpg-agent forwarding to the selected host |
| `d` | Run `doctor` on the selected host |
| `r` | Reload the config file |
| `l` | Show or hide the log |
| `?` | Help |
| `q` | Quit. Connections and forwards keep running in the daemon |

Forwards that come from a profile can't be removed on their own; deactivate
the profile (or edit the config) instead.

## Tray icon

`tether tray` puts an icon in the system tray (a StatusNotifierItem: KDE
Plasma, and most other Linux desktops; GNOME needs the AppIndicator
extension). The icon's color is the overall state:

| Color | When |
|---|---|
| green | every host is connected |
| blue | some hosts are connected, others disconnected |
| amber | a host is connecting, or connected with a failing forward or mount |
| red | a connection is failing |
| gray | nothing is connected |

The tooltip counts hosts by state, and active profiles, e.g.
`4 hosts: 2 up, 1 failed, 1 disconnected · 1/2 profiles active`. The same
summary heads the menu.

- **Left-click** opens the terminal UI.
- **The menu** has a submenu per host (connect or disconnect, its forwards
  and mounts with their state, retry, toggle gpg-agent forwarding, mount a
  directory in either direction, unmount ad-hoc mounts, and forget for
  ad-hoc hosts), your profiles as checkboxes, "Connect to host…" (asks for
  `NAME [SSH-DEST]`), and reload. Prompts use `kdialog` or `zenity`; without
  either, the terminal UI opens instead.
- **Mounting from the menu**: "Mount remote directory here…" asks for the
  remote directory and a local mount point (default `~/mnt/<name>`, created
  if missing). "Mount local directory on HOST…" opens a folder picker, then
  asks where to mount it on the remote (default `~/<name>`). Like
  `tether mount add`, this connects the host if needed.
- **Desktop notifications** say when a connection drops or comes back, and
  when a forward or mount fails. Later news about the same thing replaces the
  earlier notification. Turn them off with `--no-notify`.

If the daemon isn't running, the icon turns gray and the menu offers to start
it; the tray reconnects on its own when the daemon comes back. Quitting the
tray leaves everything running.

The terminal UI opens in `$TERMINAL`, or the first of konsole, kgx,
gnome-terminal, kitty, alacritty, foot, wezterm, xfce4-terminal and xterm
that's installed. Pick one with `--terminal`, one word per flag:
`tether tray --terminal konsole --terminal -e`.

**Start it with your desktop session.** With home-manager:

```nix
programs.tether.tray = {
  enable = true;
  # extraArgs = [ "--terminal" "konsole" "--terminal" "-e" ];
};
```

This adds a `tether-tray.service` user unit tied to your graphical session.
Without home-manager, add an autostart entry:

```ini
# ~/.config/autostart/tether-tray.desktop
[Desktop Entry]
Type=Application
Name=tether
Exec=tether tray
X-KDE-autostart-phase=2
```

## CLI

```
tether up NAME...                 connect hosts / activate profiles, wait for the result
  --host | --profile              say which, if a host and profile share a name
  --no-wait, --timeout 45s
tether down NAME...               deactivate profiles / disconnect hosts
tether host add NAME [SSH-DEST]   connect to a host that isn't in the config (ad hoc)
tether host rm NAME...            disconnect ad-hoc hosts and forget them
tether fwd add HOST SPEC...       add ad-hoc forwards (connects the host if needed)
tether fwd rm HOST SPEC...        remove ad-hoc forwards
tether gpg on HOST [--ssh]        forward gpg-agent (and its SSH socket) ad hoc
tether gpg off HOST [--ssh]       stop it
tether doctor HOST [--json]       check local, connection and remote setup
tether tui                        interactive terminal UI
tether tray [--no-notify]         system tray icon
  --terminal WORD                 terminal for the TUI (repeat per word)
tether logs [-f] [-n 50]          the daemon's recent log (it keeps 500 entries)
tether mount add HOST SRC DST     mount SRC at DST; one side is remote:PATH
  -o OPTION                       extra sshfs option (repeatable)
tether mount rm HOST SRC DST      unmount an ad-hoc mount
tether status [--json]            hosts, forwards and profiles
tether status -w [--json]         keep running and show every change
tether config check               validate the config file (no daemon needed)
tether config reload              make the daemon re-read the config
tether daemon                     run the daemon in the foreground
  --log-level LEVEL               debug, info, warn, error (default info)
  --ssh-config FILE               use FILE instead of ~/.ssh/config (ssh -F)
tether daemon stop                stop the running daemon
tether version
```

Global flags: `--socket PATH`, `--config PATH`, `--no-autostart`.

For example:

```console
$ tether up work
work: up
$ tether fwd add devbox D:1080 R:0:localhost:3000
D:1080: up
R:0:localhost:3000: up (remote port 40663)
$ tether status
daemon  0.1.0  pid 81234  up 2h13m
config  /home/you/.config/tether/config.toml

HOST    SSH     AUTO  STATE  DETAIL
devbox  devbox  yes   up

FORWARD                  HOST    FROM    STATE  DETAIL
D:1080                   devbox  ad-hoc  up
L:5432:db.internal:5432  devbox  work    up
R:0:localhost:3000       devbox  ad-hoc  up     remote port 40663

PROFILE  HOST    AUTO  STATE  DETAIL
work     devbox  -     up
```

States:

| State | Meaning |
|---|---|
| `down` | Not wanted: nothing asked for this host or profile. |
| `pending` | Connecting, or a forward is being added. |
| `up` | Connected, and every forward is working. |
| `degraded` | Connected, but some forwards failed (e.g. the local port is taken). They are retried every 15 seconds. |
| `error` | The connection failed or dropped. `DETAIL` says why and when the next attempt is. |

`up` and `fwd add` exit non-zero unless everything they asked for is `up`.
Ad-hoc forwards last until you remove them, take their host down, or stop the
daemon. For permanent ones, use a profile.

**Ad-hoc hosts** work the same way for hosts that aren't in the config:
`tether host add devbox2` connects to the ssh alias `devbox2`, and
`tether host add scratch me@10.0.0.5` names a destination. You can then add
forwards, mounts and gpg to them, and run `doctor`. `tether down` disconnects
an ad-hoc host but keeps it listed; `tether host rm` forgets it along with its
forwards and mounts. They don't survive a daemon restart; add them to the
config to keep them. If the config later defines the same name, the config's
entry wins. Besides specs, `fwd add` accepts the
names `gpg-agent` and `gpg-ssh`, which is what `tether gpg on` uses.

`tether status -w --json` prints one JSON object per change, which works
well for status bars and scripts.

## Paths and environment variables

| What | Default | Override |
|---|---|---|
| Config file | `$XDG_CONFIG_HOME/tether/config.toml` (`~/.config/...`) | `--config` or `TETHER_CONFIG` |
| Socket | `$XDG_RUNTIME_DIR/tether/tether.sock` | `--socket` or `TETHER_SOCKET` |
| SSH control sockets | `$XDG_RUNTIME_DIR/tether/ctl/<host>` | — |
| Log (on-demand daemon) | next to the socket, `daemon.log` | — |
| Log (systemd) | the journal: `journalctl --user -u tether` | — |

If `XDG_RUNTIME_DIR` isn't set, the runtime directory falls back to
`/tmp/tether-<uid>`.

## Development

```sh
nix develop              # Go, gopls, golangci-lint
go test -race ./...
go run ./cmd/tether daemon --log-level debug
nix build                # builds the package and runs the tests
nix flake check
```

Without Nix, any Go 1.26+ toolchain works for the `go` commands.

The integration tests start a throwaway, unprivileged `sshd` on loopback and
run real connections through it. They need `sshd` and `ssh-keygen` (the dev
shell provides them; elsewhere set `TETHER_TEST_SSHD` if `sshd` isn't on
`PATH` or in `/usr/sbin`). They skip themselves where sshd can't log you in,
such as the Nix build sandbox.

If you change Go dependencies, update `vendorHash` in `nix/package.nix`. Set it
to `lib.fakeHash`, run `nix build`, and copy the hash from the error message.
