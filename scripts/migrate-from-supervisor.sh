#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Migrate a Ubuntu/Debian host from Supervisor to Direktor.

Usage:
  sudo ./scripts/migrate-from-supervisor.sh [options]

Options:
  --binary-dir PATH
    Directory containing direktord and direktorctl binaries.
    Default: ./dist/linux-$(dpkg --print-architecture)
    Fallback: ./dist/linux-$(uname -m mapped to Go arch)

  --supervisor-config PATH
    Supervisor entrypoint config to migrate.
    Default: /etc/supervisor/supervisord.conf

  --target-dir PATH
    Direktor config root.
    Default: /etc/direktor

  --service-name NAME
    Supervisor systemd service name.
    Default: supervisor

  --cutover
    Stop and disable Supervisor, then enable and start Direktor.

  --convert
    Convert the Supervisor config tree into native Direktor files under
    TARGET_DIR using the Go converter. Requires a working Go toolchain.
    Without this flag, Direktor runs the copied Supervisor-compatible config.

  --dry-run
    Print actions without changing the system.

Examples:
  sudo ./scripts/migrate-from-supervisor.sh --cutover
  sudo ./scripts/migrate-from-supervisor.sh --convert --cutover
  sudo ./scripts/migrate-from-supervisor.sh --binary-dir ./dist/linux-amd64
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SUPERVISOR_CONFIG="/etc/supervisor/supervisord.conf"
TARGET_DIR="/etc/direktor"
SERVICE_NAME="supervisor"
DO_CUTOVER=0
DO_CONVERT=0
DRY_RUN=0

log() {
  printf '[migrate] %s\n' "$*"
}

die() {
  printf '[migrate] error: %s\n' "$*" >&2
  exit 1
}

run() {
  if (( DRY_RUN )); then
    printf '[dry-run] '
    printf '%q ' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

need_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    die "run as root"
  fi
}

detect_arch() {
  local dpkg_arch
  if command -v dpkg >/dev/null 2>&1; then
    dpkg_arch="$(dpkg --print-architecture)"
    case "$dpkg_arch" in
      amd64) printf 'amd64\n'; return 0 ;;
      arm64) printf 'arm64\n'; return 0 ;;
    esac
  fi

  case "$(uname -m)" in
    x86_64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *)
      die "unsupported architecture $(uname -m); pass --binary-dir explicitly"
      ;;
  esac
}

install_binaries() {
  local binary_dir="$1"
  [[ -x "$binary_dir/direktord" ]] || die "missing $binary_dir/direktord"
  [[ -x "$binary_dir/direktorctl" ]] || die "missing $binary_dir/direktorctl"

  log "installing binaries from $binary_dir"
  run install -Dm755 "$binary_dir/direktord" /usr/local/bin/direktord
  run install -Dm755 "$binary_dir/direktorctl" /usr/local/bin/direktorctl
}

backup_existing_state() {
  local backup_root="$1"

  log "creating backup under $backup_root"
  run mkdir -p "$backup_root"

  if [[ -f "$SUPERVISOR_CONFIG" ]]; then
    run cp -a "$(dirname "$SUPERVISOR_CONFIG")" "$backup_root/supervisor"
  fi
  if [[ -d "$TARGET_DIR" ]]; then
    run cp -a "$TARGET_DIR" "$backup_root/direktor"
  fi
  if [[ -f /etc/systemd/system/direktord.service ]]; then
    run cp -a /etc/systemd/system/direktord.service "$backup_root/direktord.service"
  fi
  if [[ -f /etc/systemd/system/"$SERVICE_NAME".service ]]; then
    run cp -a /etc/systemd/system/"$SERVICE_NAME".service "$backup_root/"
  fi
}

stage_compat_config() {
  local config_dir
  config_dir="$(dirname "$SUPERVISOR_CONFIG")"

  log "staging Supervisor-compatible config under $TARGET_DIR/supervisor"
  run mkdir -p "$TARGET_DIR"
  run rm -rf "$TARGET_DIR/supervisor"
  run cp -a "$config_dir" "$TARGET_DIR/supervisor"

  write_env_file "$TARGET_DIR/supervisor/$(basename "$SUPERVISOR_CONFIG")"
}

convert_config() {
  local output_dir="$TARGET_DIR"

  command -v go >/dev/null 2>&1 || die "--convert requires Go to be installed"
  [[ -f "$SUPERVISOR_CONFIG" ]] || die "Supervisor config not found at $SUPERVISOR_CONFIG"

  log "converting Supervisor config to native Direktor layout in $output_dir"
  run mkdir -p "$TARGET_DIR"
  run go run "$REPO_ROOT/scripts/convert-supervisor" \
    --input "$SUPERVISOR_CONFIG" \
    --output-dir "$output_dir"

  write_env_file "$TARGET_DIR/direktor.conf"
}

write_env_file() {
  local config_path="$1"

  log "writing /etc/direktor/direktord.env for $config_path"
  if (( DRY_RUN )); then
    printf '[dry-run] write %s\n' "/etc/direktor/direktord.env"
    return 0
  fi

  mkdir -p /etc/direktor
  cat > /etc/direktor/direktord.env <<EOF
# Managed by migrate-from-supervisor.sh
DIREKTORD_CONFIG=$config_path
EOF
}

install_service_files() {
  log "installing systemd service files"
  run install -Dm644 "$REPO_ROOT/deploy/systemd/direktord.service" /etc/systemd/system/direktord.service
  if [[ ! -f /etc/direktor/direktord.env ]]; then
    run install -Dm644 "$REPO_ROOT/deploy/etc/direktor/direktord.env" /etc/direktor/direktord.env
  fi
  run install -d /var/log/direktor
  run systemctl daemon-reload
}

verify_config() {
  local config_path
  config_path="$(awk -F= '/^DIREKTORD_CONFIG=/{print $2}' /etc/direktor/direktord.env 2>/dev/null || true)"
  [[ -n "$config_path" ]] || config_path="$TARGET_DIR/direktor.conf"

  log "verifying Direktor can start with $config_path"
  if (( DRY_RUN )); then
    printf '[dry-run] /usr/local/bin/direktord -n -c %q\n' "$config_path"
    return 0
  fi

  timeout 10s /usr/local/bin/direktord -n -c "$config_path" >/tmp/direktor-migrate-verify.log 2>&1 || {
    local status=$?
    if [[ $status -ne 124 ]]; then
      cat /tmp/direktor-migrate-verify.log >&2 || true
      die "Direktor verification failed"
    fi
  }
  rm -f /tmp/direktor-migrate-verify.log
}

cutover_services() {
  log "cutting over from $SERVICE_NAME to direktord"

  if systemctl list-unit-files | grep -q "^${SERVICE_NAME}\.service"; then
    run systemctl stop "$SERVICE_NAME"
    run systemctl disable "$SERVICE_NAME"
  else
    log "service $SERVICE_NAME not found in systemd unit list; skipping stop/disable"
  fi

  run systemctl enable direktord
  run systemctl restart direktord
  run systemctl --no-pager --full status direktord
}

print_next_steps() {
  cat <<EOF

Migration staging complete.

Current Direktor config source:
  $(awk -F= '/^DIREKTORD_CONFIG=/{print $2}' /etc/direktor/direktord.env 2>/dev/null || printf '%s' "$TARGET_DIR/direktor.conf")

Useful checks:
  direktorctl status
  journalctl -u direktord -n 100 --no-pager
  systemctl status direktord

If you did not use --cutover yet:
  1. Review the staged config under $TARGET_DIR
  2. Run: sudo systemctl stop $SERVICE_NAME
  3. Run: sudo systemctl disable $SERVICE_NAME
  4. Run: sudo systemctl enable --now direktord

Rollback:
  1. sudo systemctl stop direktord
  2. sudo systemctl enable --now $SERVICE_NAME
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary-dir)
      BINARY_DIR="${2:-}"
      shift 2
      ;;
    --supervisor-config)
      SUPERVISOR_CONFIG="${2:-}"
      shift 2
      ;;
    --target-dir)
      TARGET_DIR="${2:-}"
      shift 2
      ;;
    --service-name)
      SERVICE_NAME="${2:-}"
      shift 2
      ;;
    --cutover)
      DO_CUTOVER=1
      shift
      ;;
    --convert)
      DO_CONVERT=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

ARCH="$(detect_arch)"
BINARY_DIR="${BINARY_DIR:-$REPO_ROOT/dist/linux-$ARCH}"
SUPERVISOR_CONFIG="$(readlink -f "$SUPERVISOR_CONFIG" 2>/dev/null || printf '%s' "$SUPERVISOR_CONFIG")"
TARGET_DIR="$(readlink -m "$TARGET_DIR" 2>/dev/null || printf '%s' "$TARGET_DIR")"
BACKUP_ROOT="/var/backups/direktor-migrate-$(date -u +%Y%m%dT%H%M%SZ)"

need_root
[[ -f "$SUPERVISOR_CONFIG" ]] || die "Supervisor config not found at $SUPERVISOR_CONFIG"

install_binaries "$BINARY_DIR"
backup_existing_state "$BACKUP_ROOT"

if (( DO_CONVERT )); then
  convert_config
else
  stage_compat_config
fi

install_service_files
verify_config

if (( DO_CUTOVER )); then
  cutover_services
fi

print_next_steps
