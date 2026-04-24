# zed-remote-wrapper

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**zed-remote-wrapper** bridges the gap between a remote SSH session and your local
[Zed](https://zed.dev) editor, enabling you to run `zed` CLI commands directly
on a remote host and have them open seamlessly in your local Zed instance.
Supports macOS and Linux on both local and remote sides.

## How it works

Zed ships native SSH Remote Development out of the box — run `zed ssh://alias/path`
locally and Zed pushes a remote server over SSH, giving you LSP, terminal,
debugger, and AI on the remote machine. However, the entry point is the
**local** CLI only. There is no built-in way to type `zed foo` inside a remote
terminal session and have your local Zed open it.

**zed-remote-wrapper** solves this by forwarding the `zed` command through the SSH
tunnel back to the local machine, using a Unix domain socket on both ends
bridged by SSH `RemoteForward`.

```
  [Remote host]                      [SSH tunnel]            [Local host]
    (wrapper)
        │ JSON over unix socket
        ▼
  /tmp/zed-$USER-$HOST.sock  ══════  RemoteForward  ══════  ~/.zed-remote.sock
                                                                   │
                                                                   ▼
                                                               (listener)
                                                                   │ exec
                                                                   ▼
                                                            zed ssh://alias/path
```

## Prerequisites

### Local

- **Zed with CLI** — `zed` must be on `$PATH`.
  - macOS: open Zed and run `Zed → Install CLI` from the menu bar.
  - Linux: install via the [official script](https://zed.dev/docs/linux) or your package manager; the CLI is included.
  - You can verify with `which zed && zed --version`; you should see a path like `/usr/local/bin/zed` and a version string.
- **OpenSSH ≥ 7.6** — required for `RemoteForward` with Unix domain socket support.
  - You can verify with `ssh -V`; you should see `OpenSSH_7.6` or higher.

### Remote

- **`sshd` accepts `LC_ZED_REMOTE_*` env vars** — the wrapper relies on four
  variables (`LC_ZED_REMOTE_HOST`, `LC_ZED_REMOTE_USER`, `LC_ZED_REMOTE_PORT`,
  `LC_ZED_REMOTE_SOCK`) to build fully-qualified `ssh://user@host:port/path` URLs.
  Most distributions default to `AcceptEnv LANG LC_*`, which covers all of them.
  If your sshd does not accept them, see [Config Fallback](#config-fallback)
  for alternatives.
  - On the remote host, run `grep AcceptEnv /etc/ssh/sshd_config`; you should see
    a line containing `LC_*`.
- **`~/.local/bin` on `$PATH`** — the wrapper binary (`zed`) is installed here by default.
  - On the remote host, run `echo $PATH`; the output should contain `~/.local/bin` or
    `/home/<user>/.local/bin`.

## Install

### Pre-built binaries

_(Installation script coming soon.)_

### Build from source

```bash
make build
```

To cross-compile for a specific platform:

```bash
GOOS=linux GOARCH=arm64 go build -o <out> ./cmd/wrapper
```

All binaries are generated into directory `dist`.

## Usage

### 1. Start the listener (local)

```bash
# Run directly
zed-remote-listener -v

# Run by tmux (recommended)
tmux new -d -s zed-remote 'zed-remote-listener -v'
```

You'll know it's running when `ls -l ~/.zed-remote.sock` shows an `srw-------` socket.

### 2. Configure SSH (local)

Add the following block to your local `~/.ssh/config`:

```
# >>> zed-remote >>>
Match exec "test -S %d/.zed-remote.sock"
  SetEnv LC_ZED_REMOTE_HOST=%n
  SetEnv LC_ZED_REMOTE_USER=%r
  SetEnv LC_ZED_REMOTE_PORT=%p
  SetEnv LC_ZED_REMOTE_SOCK=/tmp/zed-%r-%h.sock
  RemoteForward /tmp/zed-%r-%h.sock %d/.zed-remote.sock
  StreamLocalBindUnlink yes
  ExitOnForwardFailure no
# <<< zed-remote <<<
```

This block does following things:

- **Activates only when the listener is running** (`Match exec "test -S …"`).
- **Injects connection details** — four `SetEnv` lines capture the SSH host alias
  (`%n`), remote username (`%r`), remote port (`%p`), and socket path, so the
  listener can build fully-qualified `ssh://user@host:port/path` URLs for Zed.
- **Injects the socket path** (`SetEnv LC_ZED_REMOTE_SOCK=/tmp/zed-%r-%h.sock`) — the
  wrapper reads this to locate the right socket.
- **Forwards the socket** (`RemoteForward`) — tunnels the local listener socket to
  `/tmp/zed-<user>-<host>.sock` on the remote. `StreamLocalBindUnlink yes` cleans up
  stale sockets from previous sessions; `ExitOnForwardFailure no` keeps the session
  alive even if the forward fails.

### 3. Deploy the wrapper (remote)

**Using the install script** — the binary is placed at `~/.local/bin/zed` automatically.
Ensure `~/.local/bin` is on your `$PATH` and you're ready to go.

**Using a manually built binary** — copy the binary to any directory on your `$PATH`
and rename it to `zed`:

```bash
cp dist/zed-remote-wrapper-linux-amd64 ~/.local/bin/zed
```

### 4. Open files from the remote host

SSH into the remote host and use `zed` as you would locally:

```bash
zed path/to/file              # open a single file
zed -n .                      # open cwd in a new workspace
zed -a extra.txt              # add to the current workspace
zed -w Makefile               # wait until the window closes (useful as $EDITOR)
zed src/main.go:42:7          # jump to line 42, column 7
zed --diff old.txt new.txt    # open a diff view
```

## Config Fallback

When `LC_ZED_REMOTE_*` variables are unavailable — for example when using `sudo`,
`su`, cron jobs, containers, or an sshd that restricts `LC_*` forwarding — the
wrapper falls back to config files. All four variables follow the same resolution
order: **env var → `~/.config/zed-remote.conf` → `/etc/zed-remote.conf`**.

| Variable    | Env var               | Conf key | If unset                         |
| ----------- | --------------------- | -------- | -------------------------------- |
| Host alias  | `$LC_ZED_REMOTE_HOST` | `host=`  | error — required                 |
| Remote user | `$LC_ZED_REMOTE_USER` | `user=`  | omitted from URL                 |
| Remote port | `$LC_ZED_REMOTE_PORT` | `port=`  | defaults to 22, omitted from URL |
| Socket path | `$LC_ZED_REMOTE_SOCK` | `sock=`  | error — required                 |

### Config file format

```
host=myhost
user=myuser
port=2222
sock=/tmp/zed-myuser-myhost.sock
# Lines starting with # are comments.
```

`user=`, `port=`, and `sock=` are _static_ values — they do not vary per connection.

If `port=` is unset (and `LC_ZED_REMOTE_PORT` is absent), the port defaults to 22
and is omitted from the generated `ssh://` URL.

## Protocol

The wire protocol is newline-framed JSON.

### Request (wrapper → listener)

```json
{
  "v": 1,
  "host": "myhost",
  "cwd": "/home/me",
  "paths": [{ "path": "/abs/x.go", "line": 12, "col": 3 }],
  "wait": true,
  "diffs": [{ "a": "/abs/a", "b": "/abs/b" }]
}
```

| Field      | Type   | Description                                                               |
| ---------- | ------ | ------------------------------------------------------------------------- |
| `v`        | int    | Protocol version. Currently `1`.                                          |
| `host`     | string | SSH host alias used in the `ssh://alias/path` URL.                        |
| `cwd`      | string | Working directory on the remote host.                                     |
| `paths`    | array  | Files to open. Each entry has `path`; `line` and `col` are optional.      |
| `wait`     | bool   | Wait for the Zed window to close before returning. Omitted when `false`.  |
| `add`      | bool   | Add paths to the current workspace. Omitted when `false`.                 |
| `new`      | bool   | Open in a new workspace. Omitted when `false`.                            |
| `existing` | bool   | Reuse an existing workspace. Omitted when `false`.                        |
| `diffs`    | array  | Diff pairs to open. Each entry has `a` and `b` paths. Omitted when empty. |

### Response (listener → wrapper)

```json
{"t": "out", "d": "<base64>"}
{"t": "err", "d": "<base64>"}
{"t": "exit", "code": 0}
{"t": "error", "msg": "something went wrong"}
```

| `t`     | Fields         | Description                                                |
| ------- | -------------- | ---------------------------------------------------------- |
| `out`   | `d` (base64)   | A chunk of the local `zed` process's stdout.               |
| `err`   | `d` (base64)   | A chunk of its stderr.                                     |
| `exit`  | `code` (int)   | `zed` exited; the listener then closes the connection.     |
| `error` | `msg` (string) | Listener-side failure (parse/exec); no `exit` will follow. |

## Troubleshooting

| Symptom                                             | What to check                                                                                                                                                                               |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `zed-remote: host not configured`                   | Host alias not resolved. Check `echo $LC_ZED_REMOTE_HOST`; if empty, verify `AcceptEnv LC_*` in remote `sshd_config`, or write `host=ALIAS` to `~/.config/zed-remote.conf`.                 |
| `zed-remote: /tmp/zed-$USER-$HOST.sock not present` | `RemoteForward` failed. Confirm the local listener is running and check `ssh -vvv <alias> true` for forwarding details. Also verify `AcceptEnv LC_ZED_REMOTE_SOCK` in remote `sshd_config`. |
| `zed-remote: listener closed connection`            | Listener crashed or was stopped. Run it in the foreground with `zed-remote-listener -v` to see errors.                                                                                      |
| `Could not request local forwarding.`               | Listener is not running, or the `Match exec` gate failed. Verify the socket path and OpenSSH version.                                                                                       |
| Zed shows `ssh://...` tabs stuck "connecting"       | Configure the host in Zed once via the command palette (`project: open remote ssh...`) so Zed knows the key and port.                                                                       |
| `bind: Address already in use`                      | A stale socket remains. Run `rm ~/.zed-remote.sock` and restart the listener.                                                                                                               |

## Uninstall

```bash
# Local
pkill -f zed-remote-listener
rm -f ~/.local/bin/zed-remote-listener ~/.zed-remote.sock

# Remove the `# >>> zed-remote >>>` … `# <<< zed-remote <<<` block from ~/.ssh/config

# Remote (per host)
ssh <alias> 'rm -f ~/.local/bin/zed ~/.config/zed-remote.conf'
```

## Limitations

- macOS and Linux only — no Windows support on either side.
- The listener does not auto-restart on crash.
- **Multiple concurrent sessions to the same host** — each SSH session binds
  `RemoteForward` to the same socket path (`/tmp/zed-<user>-<host>.sock`); because
  `StreamLocalBindUnlink yes` is set, each new session replaces the previous binding.
  When any session disconnects, sshd removes the socket file, causing new `zed`
  invocations from remaining sessions to fail with "socket not present".
