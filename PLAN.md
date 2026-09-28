# tether — forwarding manager for remote development

One binary, `tether`. `tether daemon` runs the daemon; every other subcommand is a client.

## Decisions

| Topic | Decision |
|---|---|
| Name | `tether` (no clashes in nixpkgs or with tunnel tools; `burrow`/`hitch` rejected for clashes) |
| Language | Go |
| SSH layer | Wrap system OpenSSH (one ControlMaster per host) |
| Mounts | Live FUSE mounts (sshfs; reverse sshfs for local→remote) |
| Platform (local side) | Linux only (systemd --user, D-Bus, FUSE available) |
| Remote requirements | `sshd` only; `sshfs` + FUSE only for local→remote mounts; `usbip` + `vhci-hcd` + sudo only for USB. No remote agent. |
| Local machine | Fedora + Nix + home-manager (primary). Also installable without Nix (`go install` / release binary). |
| Remotes | Mostly NixOS (we ship a NixOS module for remote-side setup), some Debian (`doctor` prints manual fixes). |
| GPG | Commit signing, `pass`/decryption, **and** gpg-agent as the SSH agent (forward the ssh socket too). |
| Bastion / multi-hop | `ProxyJump` plus forwards to third-party hosts; no custom hop chains for now |
| home-manager | Installs package + systemd user unit; generates `config.toml` only if `settings` is set, otherwise leaves a hand-edited file alone |
| Autoconnect | Per host or profile `autoconnect` flag (start at login, reconnect on network change); otherwise on demand. |

## Architecture

```
 tether (CLI)   tether tui   tray (future)
       \            |            /
        Unix socket API  ($XDG_RUNTIME_DIR/tether/tether.sock)
        JSON-RPC 2.0 requests + server-pushed event stream
                    |
               tether daemon (systemd --user; CLI can also start it on demand)
               ├── config loader + watcher (TOML)
               ├── state store (desired vs. actual)
               ├── reconciler / supervisor (backoff, health checks, netlink/NM watch)
               └── per-host session
                    ├── ControlMaster process   ssh -MNf -S <sock> <host>
                    ├── forwards                ssh -S <sock> -O forward|cancel
                    ├── gpg forward             unix RemoteForward
                    └── mounts                  sshfs / reverse-sshfs child processes
```

### Core ideas
- **The daemon owns all state.** CLI, TUI and tray are thin clients of the same API, so adding a GUI later means writing only a new client.
- **Declarative config plus a reconciler.** The config and ad-hoc CLI commands make up the *desired* state. The daemon keeps the *actual* state matching it and heals it after drops, reconnects and network changes.
- **Reuse the user's `~/.ssh/config`.** A host is usually just an ssh alias, so bastion hosts come from `ProxyJump`. Keys, FIDO, known_hosts and agents all work unchanged.
- **Each item has an ID and a status** (`pending | up | degraded | down | error(msg)`), streamed to clients as events.

## Feature mechanics

### 1. Port forwarding
- Local `L:[bind:]port:host:hostport`, remote `R:[bind:]port:host:hostport`, dynamic `D:[bind:]port` (SOCKS).
- Added and removed live on the master: `ssh -S sock -O forward -L ...` / `-O cancel`.
- Bastion hosts: `ProxyJump` in ssh config, or a forward whose target is a third-party host reachable from the remote.
- Unix-socket forwards are supported too (useful for docker.sock, etc.).
- Before adding a forward, check whether the local port is already in use and report a clear error.

### 2. GPG agent forwarding
- Local side: `gpgconf --list-dirs agent-extra-socket`.
- Remote side: run `gpgconf --list-dirs agent-socket` over the master once and cache the result.
- Before forwarding, remove the stale remote socket (`rm -f`) so that `StreamLocalBindUnlink yes` is not *required* in the remote sshd config (still recommended).
- `tether doctor <host>` checks that the remote gpg-agent socket activation is masked, the public keys are present on the remote, and the socket is reachable.
- **SSH via gpg-agent (`gpg_ssh`):** forward the local `agent-ssh-socket` to the remote's own `agent-ssh-socket` path. The remote shell points `SSH_AUTH_SOCK` there (NixOS module, or a snippet for Debian).
- **Stop the remote's own agent before binding.** Removing its socket isn't enough: gpg-agent notices, exits, and deletes the socket path, taking the forward with it. `gpgconf --kill` through a stale forward is refused by the local extra socket (tested).
- Remote-side NixOS module (`tether.remote.enable`): sets `StreamLocalBindUnlink yes`, disables gpg-agent socket activation for the user, enables FUSE `user_allow_other` if needed, and sets `SSH_AUTH_SOCK`.

### 3. Mounts (live FUSE)
- **remote→local:** `sshfs -o ssh_command='ssh -S <ctlsock>' host:path localpath`, reusing the master connection.
- **local→remote:** the remote runs `sshfs -o passive`, served over the session's stdin/stdout by an SFTP server inside the daemon (`internal/sftpjail`, `pkg/sftp` + `os.Root`) confined to the shared directory. OpenSSH's `sftp-server` would expose everything the user can read.
- **remote→local:** the local `sshfs -o passive` is piped straight to an `ssh -s sftp` channel on the master; no second connection.
- Unmount cleanly with `fusermount3 -u` on stop, and lazily when the connection dies. On reconnect, stale mount points are detected and cleaned up.
- The `sshfs` flags are set per mount (`reconnect`, `ServerAliveInterval`, cache options).
- **Fedora + Nix gotcha:** an `sshfs` built by Nix on a non-NixOS system still needs the setuid `/usr/bin/fusermount3` from the host. Resolve `fusermount3` from the system path first and check this in `doctor`.

### 4. USB forwarding (USB/IP)
- **Privileged helper** (`tether usbip-helper`, root system service; `internal/usbip`): binds devices to `usbip-host` and serves the USB/IP setup protocol itself on `127.0.0.1:3240` instead of running `usbipd` (which can't bind to loopback only). An import hands the accepted socket to the kernel through `usbip_sockfd`.
- **Access control:** the daemon talks to the helper over a Unix socket checked with `SO_PEERCRED`; USB/IP connections are accepted only from allowed users, found by looking the peer socket up in `/proc/net/tcp`, and each user can import only the devices it exported.
- **Lifetime:** exports are tied to the daemon's control connection, so devices come back to this machine when the daemon detaches them, disconnects or dies.
- **Daemon (unprivileged):** lists devices from sysfs (polled every 2s; also how it notices a dropped attachment, from `usbip_status`), adds `R:127.0.0.1:0:127.0.0.1:<helper port>` on the host's master, and runs `sudo -n usbip --tcp-port <port> attach -r 127.0.0.1` on the remote. Detach finds the vhci port in `usbip port`.
- Remote needs `usbip`, `vhci-hcd` and passwordless sudo for usbip (NixOS: `tether.remote.usb`). Devices are named by bus ID or `vendor:product`.

## Config (`~/.config/tether/config.toml`)

```toml
[defaults]
reconnect_backoff = "1s..60s"

[hosts.devbox]
ssh = "devbox"              # ~/.ssh/config alias
autoconnect = false

[profiles.work]
host = "devbox"
gpg = true
forwards = [
  "L:5432:db.internal:5432",
  "R:8080:localhost:3000",
  "D:1080",
]
[[profiles.work.mounts]]
direction = "remote-to-local"
remote = "~/src"
local = "~/mnt/devbox/src"
```

- A profile is a named bundle of forwards, gpg and mounts on one host. Several profiles can be active on one host and share its master connection.
- Runtime/ad-hoc items are marked `ephemeral` and are not written back to the config unless `--save` is given.

## CLI surface (draft)

```
tether up <profile|host>          tether down <profile|host>
tether status [--json] [-w]       tether tui
tether fwd add <host> L:5432:db:5432 [--save]
tether fwd rm <id>
tether mount <host> remote:~/src ~/mnt/src
tether gpg on|off <host>
tether doctor <host>              tether logs [-f]
tether daemon                     tether daemon stop
tether config check|reload        tether version
```

## Project layout

```
cmd/tether/          single binary: CLI, `daemon` subcommand, TUI (cobra)
internal/api/        method names + result types shared by daemon and clients
internal/rpc/        minimal JSON-RPC 2.0 over newline-delimited JSON
internal/paths/      XDG config/runtime paths
internal/client/     socket client library (used by CLI, TUI, tray)
internal/config/     TOML schema, validation, watcher
internal/daemon/     server, state store, reconciler
internal/ssh/        ControlMaster mgmt, -O forward/cancel, remote exec
internal/forward/    forward spec parsing + lifecycle
internal/gpg/        gpg socket discovery + forwarding
internal/mount/      sshfs + reverse-sshfs
internal/usbip/      USB/IP: sysfs devices, the privileged helper, remote scripts
internal/tui/        Bubble Tea app
nix/                 package, home-manager module (flake.nix at the root)
```

Libraries: `cobra` (CLI), `bubbletea`/`lipgloss`/`bubbles` (TUI), `BurntSushi/toml` or `pelletier/go-toml/v2`, `log/slog`, `godbus/dbus` (NetworkManager signals and, later, the tray).

## Phases

0. ✅ **Skeleton:** Go module, flake devshell, `tether daemon`, socket API with `status`, config loading, systemd user unit.
1. ✅ **SSH plus forwards:** master lifecycle, L/R/D forwards, reconnect and backoff, event stream, `up`/`down`/`fwd`/`status -w`.
2. ✅ **GPG:** socket discovery, forwarding, `doctor`.
3. ✅ **TUI:** host and profile tree, live status, toggles, log pane.
4. ✅ **Mounts:** sshfs in both directions, cleanup and recovery.
5. ✅ **Tray:** KDE StatusNotifierItem client (`tether tray`, via `fyne.io/systray`), desktop notifications, home-manager `programs.tether.tray`.
6. ✅ **USB/IP:** privileged helper, `tether usb`, profile `usb = [...]`, TUI and tray sections, doctor checks, NixOS modules.

## Testing
- Unit tests: spec parsing, config validation, reconciler (with a fake SSH layer behind an interface).
- Integration tests: an `sshd` container or a NixOS VM test (`nixosTest`) with two nodes, covering forwards, gpg and sshfs from start to finish.

## Deferred / follow-ups
- `fwd add --save` (write ad-hoc forwards into the config) — needs comment-preserving TOML edits.
- Interactive auth for the daemon (`SSH_ASKPASS`, e.g. ksshaskpass) for hosts that need a password or 2FA; today auth must be non-interactive.
- Status-bar friendly output exists (`status -w --json`); a TUI comes in phase 3.

## Open questions
- None currently.
