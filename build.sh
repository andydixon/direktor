#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="${DIST_DIR:-$ROOT_DIR/dist}"
VERSION="${VERSION:-}"

mkdir $DIST_DIR 2>/dev/null || true

usage() {
  cat <<'EOF'
Direktor build and deploy helper

Usage:
  ./BUILD.sh help
  ./BUILD.sh build
  ./BUILD.sh build-current
  ./BUILD.sh install-linux
  ./BUILD.sh deploy-notes

Commands:
  help
    Show this help text.

  build
    Build all release targets:
    linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/{amd64,arm64}

  build-current
    Build only the current machine target.

  install-linux
    Build the current Linux target, install binaries to /usr/local/bin,
    install config to /etc/direktor, install the systemd unit, reload systemd,
    enable the service, and restart it.

  deploy-notes
    Print the manual Linux deployment steps without changing the system.

Environment:
  VERSION
    Optional version string embedded into both binaries.
    Example: VERSION=v0.1.0 ./BUILD.sh build

  DIST_DIR
    Optional output directory for build artifacts.
    Example: DIST_DIR=/tmp/direktor ./BUILD.sh build

Examples:
  ./BUILD.sh build
  VERSION=v0.1.0 ./BUILD.sh build-current
  sudo ./BUILD.sh install-linux
EOF
}

build_flags() {
  local flags=("--output" "$DIST_DIR")
  if [[ -n "$VERSION" ]]; then
    flags+=("--version" "$VERSION")
  fi
  printf '%s\n' "${flags[@]}"
}

run_build() {
  local -a flags
  mapfile -t flags < <(build_flags)
  (cd "$ROOT_DIR" && go run ./scripts/build "${flags[@]}" "$@")
}

current_target() {
  local goos goarch
  goos="$(go env GOOS)"
  goarch="$(go env GOARCH)"
  printf '%s/%s\n' "$goos" "$goarch"
}

manual_deploy_notes() {
  cat <<'EOF'
Manual Linux deploy steps

1. Build the native Linux binaries:
   ./BUILD.sh build-current

2. Install binaries:
   sudo install -Dm755 dist/linux-$(go env GOARCH)/direktord /usr/local/bin/direktord
   sudo install -Dm755 dist/linux-$(go env GOARCH)/direktorctl /usr/local/bin/direktorctl

3. Install configuration:
   sudo install -d /etc/direktor/conf.d
   sudo install -Dm644 deploy/etc/direktor/direktor.conf /etc/direktor/direktor.conf
   sudo install -Dm644 deploy/etc/direktor/direktord.env /etc/direktor/direktord.env
   sudo install -Dm644 deploy/etc/direktor/conf.d/example-app.conf /etc/direktor/conf.d/example-app.conf

4. Install the systemd unit:
   sudo install -Dm644 deploy/systemd/direktord.service /etc/systemd/system/direktord.service

5. Create the log directory:
   sudo install -d /var/log/direktor

6. Edit /etc/direktor/conf.d/*.conf for your real programs.

7. Enable and start the service:
   sudo systemctl daemon-reload
   sudo systemctl enable direktord
   sudo systemctl restart direktord
   sudo systemctl status direktord

8. Control the supervisor:
   direktorctl status
EOF
}

install_linux() {
  if [[ "$(go env GOOS)" != "linux" ]]; then
    echo "install-linux must be run on Linux" >&2
    exit 1
  fi

  local target arch
  target="$(current_target)"
  arch="${target#linux/}"

  run_build --targets "$target"

  install -d /usr/local/bin
  install -d /etc/direktor/conf.d
  install -d /var/log/direktor

  install -m755 "$DIST_DIR/linux-$arch/direktord" /usr/local/bin/direktord
  install -m755 "$DIST_DIR/linux-$arch/direktorctl" /usr/local/bin/direktorctl
  install -m644 "$ROOT_DIR/deploy/etc/direktor/direktor.conf" /etc/direktor/direktor.conf
  install -m644 "$ROOT_DIR/deploy/etc/direktor/direktord.env" /etc/direktor/direktord.env

  if [[ ! -f /etc/direktor/conf.d/example-app.conf ]]; then
    install -m644 "$ROOT_DIR/deploy/etc/direktor/conf.d/example-app.conf" /etc/direktor/conf.d/example-app.conf
  fi

  install -m644 "$ROOT_DIR/deploy/systemd/direktord.service" /etc/systemd/system/direktord.service

  systemctl daemon-reload
  systemctl enable direktord
  systemctl restart direktord
  systemctl --no-pager --full status direktord
}

cmd="${1:-help}"
case "$cmd" in
  help|-h|--help)
    usage
    ;;
  build)
    shift || true
    run_build "$@"
    ;;
  build-current)
    shift || true
    run_build --targets "$(current_target)" "$@"
    ;;
  install-linux)
    shift || true
    install_linux "$@"
    ;;
  deploy-notes)
    manual_deploy_notes
    ;;
  *)
    echo "Unknown command: $cmd" >&2
    echo >&2
    usage >&2
    exit 1
    ;;
esac
