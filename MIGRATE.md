# Migrate From Supervisor To Direktor

This document covers a practical Ubuntu or Debian cutover from `supervisord` to
`direktord`.

The low-risk path is:

1. Build or copy the Direktor binaries onto the target host.
2. Stage your existing Supervisor config for Direktor to consume directly.
3. Verify Direktor can start with that config.
4. Cut systemd over from `supervisor.service` to `direktord.service`.
5. Optionally convert the config tree into a cleaner Direktor-native layout.

Direktor already parses Supervisor-style INI, including `[program:x]`,
`[group:x]`, and `[include]`, so format conversion is optional rather than
mandatory.

## Preconditions

- Ubuntu or Debian host with `systemd`
- Existing Supervisor install with an entrypoint config, usually:
  `/etc/supervisor/supervisord.conf`
- Built Direktor binaries available from this repo, usually:
  `dist/linux-amd64/` or `dist/linux-arm64/`
- Root access on the target machine

## Fast Path

Use the migration script in this repo:

```bash
sudo ./scripts/migrate-from-supervisor.sh --cutover
```

By default this script:

- installs `direktord` and `direktorctl` into `/usr/local/bin`
- copies the Supervisor config tree into `/etc/direktor/supervisor`
- points Direktor at the copied `supervisord.conf`
- installs the systemd unit
- verifies the config by starting `direktord` in the foreground briefly
- stops and disables `supervisor.service` if `--cutover` is supplied
- enables and starts `direktord` if `--cutover` is supplied

### Useful Options

```bash
sudo ./scripts/migrate-from-supervisor.sh --dry-run
sudo ./scripts/migrate-from-supervisor.sh --binary-dir ./dist/linux-amd64
sudo ./scripts/migrate-from-supervisor.sh --service-name supervisord --cutover
sudo ./scripts/migrate-from-supervisor.sh --convert --cutover
```

`--convert` uses the converter at
[scripts/convert-supervisor/main.go](/home/andy/direktor/scripts/convert-supervisor/main.go:89)
to generate a native Direktor layout under `/etc/direktor`.

## What The Script Actually Does

### 1. Install Binaries

The script installs:

- `/usr/local/bin/direktord`
- `/usr/local/bin/direktorctl`

from a source directory such as `dist/linux-amd64`.

### 2. Back Up Existing State

Before changing anything, it creates a timestamped backup under:

```text
/var/backups/direktor-migrate-YYYYMMDDTHHMMSSZ
```

This includes any existing Direktor config and the Supervisor config directory.

### 3. Stage Config

Default mode copies the whole Supervisor config directory into:

```text
/etc/direktor/supervisor
```

Then it writes:

```text
/etc/direktor/direktord.env
```

with:

```ini
DIREKTORD_CONFIG=/etc/direktor/supervisor/supervisord.conf
```

This matters because it preserves include paths and lets you test Direktor
without rewriting the original config first.

### 4. Install The Service

The systemd unit comes from:

- [deploy/systemd/direktord.service](/home/andy/direktor/deploy/systemd/direktord.service:1)
- [deploy/etc/direktor/direktord.env](/home/andy/direktor/deploy/etc/direktor/direktord.env:1)

It runs Direktor in the foreground under systemd:

```text
/usr/local/bin/direktord -n -c ${DIREKTORD_CONFIG}
```

### 5. Verify Before Cutover

The script runs a short foreground startup test:

```bash
timeout 10s /usr/local/bin/direktord -n -c <config>
```

If Direktor exits immediately with a real error, the migration stops there.
If it stays up until `timeout` kills it, that counts as a successful preflight.

### 6. Cut Over Services

With `--cutover`, the script runs the equivalent of:

```bash
sudo systemctl stop supervisor
sudo systemctl disable supervisor
sudo systemctl enable direktord
sudo systemctl restart direktord
```

If your unit is named differently, pass `--service-name`.

## Recommended Migration Sequence

### Option A: Safest Path

1. Build Direktor on a matching machine:

```bash
./build.sh build-current
```

2. Copy the repo or at least the built binaries and deployment files to the
target host.

3. Run a dry run:

```bash
sudo ./scripts/migrate-from-supervisor.sh --dry-run
```

4. Stage everything without cutover:

```bash
sudo ./scripts/migrate-from-supervisor.sh
```

5. Check the staged config path:

```bash
sudo cat /etc/direktor/direktord.env
```

6. Review the verification and service logs:

```bash
sudo journalctl -u direktord -n 100 --no-pager
```

7. When satisfied, cut over:

```bash
sudo ./scripts/migrate-from-supervisor.sh --cutover
```

### Option B: One-Step Cutover

If the host is simple and you are satisfied with the risk:

```bash
sudo ./scripts/migrate-from-supervisor.sh --cutover
```

## Native Direktor Conversion

If you want to stop carrying a Supervisor-shaped tree and move to a cleaner
Direktor layout, use:

```bash
sudo ./scripts/migrate-from-supervisor.sh --convert
```

That writes:

- `/etc/direktor/direktor.conf`
- `/etc/direktor/conf.d/*.conf`

The converter warns about unsupported Supervisor sections or keys so you can
audit any behaviour that is not portable.

## Post-Cutover Checks

After migration, verify:

```bash
sudo systemctl status direktord
direktorctl status
sudo journalctl -u direktord -n 200 --no-pager
```

Check these behaviours specifically:

- every expected process is present
- `autostart` and `autorestart` behave as intended
- `user=` directives still work
- configured stdout and stderr log paths are writable
- any included config files were copied to the staged location

## Known Risk Areas

These need explicit checking during migration:

- unsupported Supervisor extensions such as event listeners or third-party RPC
  plugins
- config keys Direktor does not implement
- service names that are not exactly `supervisor`
- log file locations under `/var/log/supervisor` with permissions tied to the
  old service model
- running both Supervisor and Direktor against the same programs at the same
  time

## Rollback

If the cutover fails:

```bash
sudo systemctl stop direktord
sudo systemctl disable direktord
sudo systemctl enable --now supervisor
```

If needed, restore the backup created under `/var/backups/direktor-migrate-*`.

## Manual Commands

If you do not want to use the script, the manual equivalent is:

```bash
sudo install -Dm755 dist/linux-amd64/direktord /usr/local/bin/direktord
sudo install -Dm755 dist/linux-amd64/direktorctl /usr/local/bin/direktorctl
sudo mkdir -p /etc/direktor
sudo cp -a /etc/supervisor /etc/direktor/supervisor
printf '%s\n' 'DIREKTORD_CONFIG=/etc/direktor/supervisor/supervisord.conf' | sudo tee /etc/direktor/direktord.env >/dev/null
sudo install -Dm644 deploy/systemd/direktord.service /etc/systemd/system/direktord.service
sudo systemctl daemon-reload
sudo timeout 10s /usr/local/bin/direktord -n -c /etc/direktor/supervisor/supervisord.conf
sudo systemctl stop supervisor
sudo systemctl disable supervisor
sudo systemctl enable --now direktord
```
