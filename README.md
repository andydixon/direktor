# Direktor

A production-grade process supervisor written in Go, functionally equivalent to
[Supervisor (supervisord)](http://supervisord.org/). Direktor manages long-running
processes with automatic restart policies, log rotation, email notifications, a
CLI control tool, and an embedded web interface.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        direktord                            │
│                                                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │   Config     │  │  Supervisor  │  │   IPC Server     │  │
│  │   Parser     │──│  Core Loop   │──│  (Unix Socket)   │  │
│  └──────────────┘  └──────┬───────┘  └────────┬─────────┘  │
│                           │                    │            │
│            ┌──────────────┼─────────┐          │            │
│            │              │         │          │            │
│  ┌─────────▼──┐ ┌────────▼──┐ ┌────▼────┐    │            │
│  │ Process A  │ │ Process B │ │ Proc C  │    │            │
│  │ (goroutine)│ │(goroutine)│ │(goroutn)│    │            │
│  └────────────┘ └───────────┘ └─────────┘    │            │
│                                               │            │
│  ┌──────────────────────┐  ┌─────────────────┐│            │
│  │   HTTP Server        │  │  Email Notifier ││            │
│  │  REST API + Web UI   │  │  (SMTP)         ││            │
│  └──────────────────────┘  └─────────────────┘│            │
└─────────────────────────────────────────────────────────────┘

┌─────────────────┐
│   direktorctl   │────── Unix Socket / Named Pipe ──────────┘
│   (CLI client)  │
└─────────────────┘
```

## Project Structure

```
direktor/
├── cmd/
│   ├── direktord/       # Daemon binary
│   │   └── main.go
│   └── direktorctl/     # CLI control tool
│       └── main.go
├── internal/
│   ├── config/          # INI config parser (Supervisor-compatible)
│   ├── process/         # Process lifecycle management
│   ├── supervisor/      # Core supervisor loop and coordination
│   ├── ipc/            # Unix socket / named pipe IPC
│   ├── logging/        # Structured logging with rotation
│   ├── notify/         # Email notifications for state changes
│   └── web/            # HTTP server, REST API, Web UI
├── pkg/
│   └── types/          # Shared types and interfaces
├── examples/
│   └── direktor.conf   # Example configuration
├── go.mod
└── README.md
```

## Building

```bash
# Show the canonical build/deploy helper
./BUILD.sh help

# Build all supported release targets:
# linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/{amd64,arm64}
./BUILD.sh build

# Build only the current machine's platform
./BUILD.sh build-current

# Custom output directory
DIST_DIR=bin ./BUILD.sh build

# Embed a version string in both binaries
VERSION=v0.1.0 ./BUILD.sh build
```

The build script produces both binaries, `direktord` and `direktorctl`, under
per-target directories such as `dist/linux-amd64/` and `dist/windows-arm64/`.

## Linux Service Layout

Sample deployment files are provided under [`deploy/`](./deploy):

- [`deploy/systemd/direktord.service`](./deploy/systemd/direktord.service)
- [`deploy/etc/direktor/direktor.conf`](./deploy/etc/direktor/direktor.conf)
- [`deploy/etc/direktor/direktord.env`](./deploy/etc/direktor/direktord.env)
- [`deploy/etc/direktor/conf.d/example-app.conf`](./deploy/etc/direktor/conf.d/example-app.conf)

This layout keeps configuration under `/etc/direktor`, while runtime state stays
under `/run/direktor` and logs under `/var/log/direktor`.

To deploy on Linux with systemd:

```bash
sudo ./BUILD.sh install-linux
```

To print the same steps without changing the system:

```bash
./BUILD.sh deploy-notes
```

## Configuration

Direktor uses an INI-style configuration file that is closely compatible with
Supervisor's format. The configuration is divided into sections, each controlling
a different aspect of the supervisor's behaviour.

### Configuration File Location

By default, `direktord` looks for its configuration at `/etc/direktor/direktor.conf`.
You may specify a different path with the `-c` flag:

```bash
direktord -c /path/to/my/direktor.conf
```

---

### `[direktord]` — Global Settings

Controls the overall behaviour of the supervisor daemon.

```ini
[direktord]
logfile = /var/log/direktor/direktord.log
loglevel = info
pidfile = /var/run/direktor.pid
nodaemon = false
minfds = 1024
minprocs = 200
identifier = direktor
socket_path = /var/run/direktor.sock
socket_mode = 0770
http_host = 127.0.0.1
http_port = 9876
http_auth = token
http_auth_token = changeme
```

| Setting         | Default                        | Description                                                              |
|-----------------|--------------------------------|--------------------------------------------------------------------------|
| `logfile`       | `/var/log/direktor/direktord.log` | Path to the supervisor's own log file                                 |
| `loglevel`      | `info`                         | Log verbosity: `debug`, `info`, `warn`, `error`                          |
| `pidfile`       | `/var/run/direktor.pid`        | Path to write the daemon's PID file                                      |
| `nodaemon`      | `false`                        | If `true`, run in the foreground (do not daemonise)                      |
| `minfds`        | `1024`                         | Minimum file descriptors the system should have available                 |
| `minprocs`      | `200`                          | Minimum number of OS processes that should be available                   |
| `identifier`    | `direktor`                     | Identifier string for this supervisor instance                           |
| `socket_path`   | `/var/run/direktor.sock`       | Path for the Unix domain socket used by `direktorctl`                    |
| `socket_mode`   | `0770`                         | File permissions for the control socket                                  |
| `http_host`     | `127.0.0.1`                    | Bind address for the built-in HTTP server                                |
| `http_port`     | `9876`                         | Port for the built-in HTTP server                                        |
| `http_auth`     | *(empty — disabled)*           | Authentication mode: `token` or `basic`                                  |
| `http_auth_token` | *(empty)*                    | The token or password used for HTTP authentication                       |

> **Note:** You may also use the section name `[supervisord]` for compatibility
> with existing Supervisor configuration files.

---

### `[program:name]` — Programme Definitions

Each `[program:x]` section defines a managed process. The section name after the
colon becomes the programme's identifier.

```ini
[program:webapp]
command = /usr/bin/gunicorn app:application -b 0.0.0.0:8000
directory = /opt/webapp
user = www-data
autostart = true
autorestart = always
startsecs = 5
startretries = 3
stopsignal = TERM
stopwaitsecs = 10
exitcodes = 0,2
priority = 100
stdout_logfile = /var/log/direktor/webapp-stdout.log
stderr_logfile = /var/log/direktor/webapp-stderr.log
stdout_logfile_maxbytes = 50MB
stdout_logfile_backups = 10
stderr_logfile_maxbytes = 50MB
stderr_logfile_backups = 10
redirect_stderr = false
environment = DJANGO_SETTINGS_MODULE="myapp.settings",SECRET_KEY="supersecret"
```

| Setting                   | Default      | Description                                                                  |
|---------------------------|--------------|------------------------------------------------------------------------------|
| `command`                 | *(required)* | The full command to execute, including arguments                              |
| `directory`               | *(none)*     | Working directory for the process                                            |
| `user`                    | *(none)*     | Run the process as this user (Unix only)                                     |
| `autostart`               | `true`       | Whether to start this programme when `direktord` starts                      |
| `autorestart`             | `always`     | Restart policy: `always`, `on-failure` (unexpected exit), or `never`         |
| `startsecs`               | `1`          | Seconds the process must stay running to be considered successfully started   |
| `startretries`            | `3`          | Maximum consecutive restart attempts before entering FATAL state             |
| `stopsignal`              | `TERM`       | Signal to send when stopping: `TERM`, `HUP`, `INT`, `QUIT`, `KILL`, `USR1`, `USR2` |
| `stopwaitsecs`            | `10`         | Grace period (seconds) before force-killing after the stop signal            |
| `exitcodes`               | `0`          | Comma-separated list of exit codes considered "expected" (for `on-failure`)  |
| `priority`                | `999`        | Start order priority (lower numbers start first)                             |
| `numprocs`                | `1`          | Number of instances to run                                                   |
| `numprocs_start`          | `0`          | Starting offset for numbered instances                                       |
| `stdout_logfile`          | `AUTO`       | Path for stdout logs (`AUTO`, `NONE`, or an absolute path)                   |
| `stderr_logfile`          | `AUTO`       | Path for stderr logs (`AUTO`, `NONE`, or an absolute path)                   |
| `stdout_logfile_maxbytes` | `50MB`       | Maximum size before rotating stdout log                                      |
| `stderr_logfile_maxbytes` | `50MB`       | Maximum size before rotating stderr log                                      |
| `stdout_logfile_backups`  | `10`         | Number of rotated stdout log files to retain                                 |
| `stderr_logfile_backups`  | `10`         | Number of rotated stderr log files to retain                                 |
| `redirect_stderr`         | `false`      | If `true`, redirect stderr into the stdout log file                          |
| `environment`             | *(none)*     | Environment variables in the format `KEY="value",KEY2="value2"`              |

---

### `[group:name]` — Programme Groups

Groups allow you to manage multiple programmes as a single unit.

```ini
[group:webstack]
programs = webapp,worker,scheduler
priority = 100
```

| Setting    | Description                                              |
|------------|----------------------------------------------------------|
| `programs` | Comma-separated list of programme names in this group    |
| `priority` | Group priority for ordered start/stop                    |

---

### `[email]` — Email Notifications

Direktor can send email alerts when managed processes change state. This is
useful for on-call notifications when a service enters FATAL or stops
unexpectedly.

```ini
[email]
enabled = true
smtp_host = smtp.example.com
smtp_port = 587
username = direktor@example.com
password = app-password-here
from = direktor@example.com
recipients = ops-team@example.com, oncall@example.com
use_tls = true
notify_on = FATAL, STOPPED, RUNNING
```

| Setting      | Default                              | Description                                                        |
|-------------|--------------------------------------|--------------------------------------------------------------------|
| `enabled`   | `false`                              | Enable or disable email notifications                              |
| `smtp_host` | *(required if enabled)*              | SMTP server hostname                                               |
| `smtp_port` | *(required if enabled)*              | SMTP port (typically 587 for TLS, 25 for unencrypted)              |
| `username`  | *(optional)*                         | SMTP authentication username                                       |
| `password`  | *(optional)*                         | SMTP authentication password                                       |
| `from`      | *(required if enabled)*              | Sender ("From") email address                                      |
| `recipients`| *(required if enabled)*              | Comma-separated list of recipient email addresses                  |
| `use_tls`   | `false`                              | Whether to use TLS for the SMTP connection                         |
| `notify_on` | `FATAL, STOPPED, EXITED, RUNNING`    | Comma-separated list of states that trigger an email               |

**Available states for `notify_on`:** `RUNNING`, `STOPPED`, `EXITED`, `FATAL`,
`BACKOFF`, `STARTING`.

Events are batched in a 5-second window to avoid email floods during cascading
failures. Each email includes the process name, old/new state, timestamp, PID,
and exit code where applicable.

> **Note:** You may also use the section name `[notify]` as an alternative to
> `[email]`.

---

### `[include]` — Including Additional Files

Split your configuration across multiple files using glob patterns:

```ini
[include]
files = /etc/direktor/conf.d/*.conf
```

Relative paths are resolved from the directory containing the main configuration
file. All matched files are parsed and merged into the main configuration.

---

## Usage

### Daemon (`direktord`)

```bash
# Start with the default configuration path
direktord

# Specify a configuration file
direktord -c /etc/direktor/direktor.conf

# Run in the foreground (useful for containers and debugging)
direktord -n

# Print version
direktord -v
```

### CLI Control (`direktorctl`)

`direktorctl` communicates with a running `direktord` instance via Unix socket
(or TCP on Windows).

```bash
# Show status of all processes
direktorctl status

# Show status of a specific process
direktorctl status myapp

# Start / stop / restart a process
direktorctl start myapp
direktorctl stop myapp
direktorctl restart myapp

# Re-read configuration from disc and apply changes
direktorctl reread
direktorctl update

# Shut down the supervisor and all managed processes
direktorctl shutdown
```

**Environment variables:**

| Variable          | Default                    | Description                                                  |
|-------------------|----------------------------|--------------------------------------------------------------|
| `DIREKTOR_SOCKET` | `/var/run/direktor.sock`   | Path to the control socket                                   |
| `DIREKTOR_COLOR`  | `auto`                     | Force colour output: `always`, `never`, or `auto` (TTY only) |
| `NO_COLOR`        | *(unset)*                  | If set to any value, disables colour output                  |

#### Status colours

When stdout is a terminal, `direktorctl status` colours the `STATUS` column to
make process state legible at a glance:

| Colour    | States                          |
|-----------|---------------------------------|
| 🟢 Green   | `RUNNING`                       |
| 🟡 Amber   | `STARTING`, `BACKOFF`, `STOPPING` |
| 🔴 Red     | `EXITED`, `FATAL`               |
| ⚪ Grey    | `STOPPED`, `UNKNOWN`            |

Colour follows the [`NO_COLOR`](https://no-color.org/) convention and is
suppressed automatically when the output is piped or redirected. Use
`DIREKTOR_COLOR=always` to keep colours when piping into a pager such as
`less -R`.

Example output:

```
$ direktorctl status
NAME          STATUS    PID     UPTIME      DESCRIPTION
webapp        RUNNING   12345   2h 15m 3s   pid 12345, uptime 2h 15m 3s
worker        RUNNING   12346   2h 15m 1s   pid 12346, uptime 2h 15m 1s
scheduler     STOPPED   -       -           exited 2024-01-15T10:30:00Z (exit code 0)
nginx-proxy   FATAL     -       -           -
```

### Web Interface

Direktor ships with an embedded web UI — no extra service to deploy, no
JavaScript build step, no external assets. The daemon serves it directly from
the same HTTP server that exposes the REST API.

**Open it in a browser at <http://127.0.0.1:9876/>** (the host and port are
controlled by `http_host` and `http_port` in the `[direktord]` section of your
configuration — see [Configuration](#direktord--global-settings) above).

To expose it beyond the local machine, set `http_host = 0.0.0.0` (or a specific
interface address) and **always** configure `http_auth` + `http_auth_token` —
the API has full control over your processes, including starting and removing
services.

```ini
[direktord]
http_host       = 0.0.0.0
http_port       = 9876
http_auth       = basic        ; or 'token'
http_auth_token = a-strong-secret
```

When `http_auth = basic`, the browser will prompt for credentials on first
visit (username can be anything; the password is `http_auth_token`). When
`http_auth = token`, programmatic clients send `Authorization: Bearer <token>`.

#### Features

- Real-time process list showing status, PID, and uptime
- **Material-design charcoal theme** with colour-coded status chips
  matching the CLI (green RUNNING, amber STARTING/BACKOFF, red EXITED/FATAL,
  grey STOPPED) and a subtle pulse on running services
- Start / Stop / Restart / Remove buttons per process
- **Add Service form** — add new processes at runtime without editing config files
- Live log viewer (stdout and stderr)
- Auto-refreshes every 5 seconds
- Keyboard accessible (Escape to close modals)

> **Tip:** the UI is just a thin client over the REST API documented below.
> If you'd rather scripted access, every action shown in the UI maps to a
> single HTTP call.

### REST API

All API endpoints return JSON. When `http_auth` is configured, include the
appropriate header with each request.

```bash
# List all processes
curl http://127.0.0.1:9876/api/processes

# Get a single process
curl http://127.0.0.1:9876/api/processes/myapp

# Start a process
curl -X POST http://127.0.0.1:9876/api/processes/myapp/start

# Stop a process
curl -X POST http://127.0.0.1:9876/api/processes/myapp/stop

# Restart a process
curl -X POST http://127.0.0.1:9876/api/processes/myapp/restart

# Add a new process at runtime
curl -X POST http://127.0.0.1:9876/api/processes \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-worker","command":"/usr/bin/worker","autorestart":"always"}'

# Remove a process (stops it first if running)
curl -X DELETE http://127.0.0.1:9876/api/processes/my-worker

# View logs (stream: stdout or stderr)
curl http://127.0.0.1:9876/api/logs/myapp?stream=stdout

# Reload configuration from disc
curl -X POST http://127.0.0.1:9876/api/reload
```

**With token authentication:**

```bash
curl -H "Authorization: Bearer changeme" http://127.0.0.1:9876/api/processes
```

**With basic authentication:**

```bash
curl -u admin:changeme http://127.0.0.1:9876/api/processes
```

#### API Reference

| Method   | Endpoint                          | Description                           |
|----------|-----------------------------------|---------------------------------------|
| `GET`    | `/api/processes`                  | List all managed processes            |
| `GET`    | `/api/processes/{name}`           | Get details for a single process      |
| `POST`   | `/api/processes/{name}/start`     | Start a stopped process               |
| `POST`   | `/api/processes/{name}/stop`      | Stop a running process                |
| `POST`   | `/api/processes/{name}/restart`   | Restart a process                     |
| `POST`   | `/api/processes`                  | Add a new process (JSON body)         |
| `DELETE`  | `/api/processes/{name}`           | Remove a process                      |
| `GET`    | `/api/logs/{name}?stream=stdout`  | Retrieve recent log output            |
| `POST`   | `/api/reload`                     | Reload configuration from disc        |

---

## Process Lifecycle

```
                    ┌──────────┐
                    │ STOPPED  │◄──── direktorctl stop
                    └────┬─────┘
                         │ start
                    ┌────▼─────┐
           ┌───────│ STARTING │
           │        └────┬─────┘
           │             │ startsecs elapsed
           │        ┌────▼─────┐
           │        │ RUNNING  │◄─── normal operation
           │        └────┬─────┘
           │             │ exit
           │        ┌────▼─────┐
    fail   │        │  EXITED  │──── autorestart=always → restart
    fast   │        └──────────┘     autorestart=on-failure → restart if unexpected
    exit   │
           │        ┌──────────┐
           ├───────►│ BACKOFF  │──── retry after exponential delay
           │        └────┬─────┘
           │             │ max retries exceeded
           │        ┌────▼─────┐
           └───────►│  FATAL   │──── manual intervention required
                    └──────────┘
```

### Restart Policies

| Policy       | Behaviour                                                              |
|-------------|------------------------------------------------------------------------|
| `always`    | Always restart the process, regardless of exit code                     |
| `on-failure`| Restart only if the exit code is not in the `exitcodes` list           |
| `never`     | Never automatically restart; the process remains in EXITED state       |

### Backoff Strategy

When a process exits before `startsecs` has elapsed, Direktor considers it a
failed start and enters BACKOFF state. It retries with an exponential delay
(1s, 2s, 3s, …). After `startretries` consecutive failures, the process enters
FATAL state and requires manual intervention (start via CLI, API, or web UI).

---

## Cross-Platform Notes

Direktor is designed to run on Linux, macOS, and Windows. Platform-specific
behaviour is handled via Go build tags.

| Feature          | Linux / macOS              | Windows                          |
|------------------|----------------------------|----------------------------------|
| IPC              | Unix domain socket         | TCP on localhost:9877            |
| Process groups   | `setpgid` for tree kill    | `CREATE_NEW_PROCESS_GROUP`       |
| Stop signal      | Configurable Unix signal   | `GenerateConsoleCtrlEvent` / Kill|
| Permissions      | Socket chmod + group       | ACLs (future)                    |
| User switching   | `setuid` / `setgid`       | Not supported                    |

---

## Synchronisation Strategy

- Each `Process` has its own `sync.RWMutex` for state protection
- The `Supervisor` holds a `sync.RWMutex` for the process map
- Process monitoring runs in dedicated goroutines per process
- IPC and HTTP handlers acquire read locks for status queries, write locks for mutations
- The `done` channel signals process exit to waiters
- Context cancellation propagates shutdown cleanly through the tree
- Email notifications are sent asynchronously via a buffered channel to avoid blocking the process manager

---

## Signals

On Unix systems, `direktord` responds to the following signals:

| Signal    | Behaviour                                           |
|-----------|-----------------------------------------------------|
| `SIGHUP`  | Reload configuration from disc (hot reload)        |
| `SIGTERM` | Graceful shutdown of all processes and the daemon   |
| `SIGINT`  | Same as `SIGTERM`                                  |

---

## Licence

This project is licensed under the GNU General Public Licence v3.0. See
[LICENCE](LICENCE) for the full text.
