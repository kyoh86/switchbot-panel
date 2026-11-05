#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "$0")"
target=${1:-minlnx1}
tmp=$(mktemp -d)
cleanup() {
  ssh -S "$tmp/ssh" -O exit "$target" >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT

# Authenticate once, using the password prompt in the caller's terminal.
ssh -M -S "$tmp/ssh" -o ControlPersist=60 -fnN "$target"
remote() { ssh -S "$tmp/ssh" "$target" "$@"; }
platform=$(remote 'uname -sm')
case "$platform" in
  'Linux x86_64') arch=amd64 ;;
  'Linux aarch64') arch=arm64 ;;
  *) echo "Unsupported server platform: $platform" >&2; exit 1 ;;
esac

go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$tmp/switchbot-panel" .
install -m 600 switchbot-panel.env "$tmp/switchbot-panel.env"
cat > "$tmp/install.sh" <<'REMOTE'
#!/bin/sh
set -eu
app="$HOME/switchbot-panel"
unit="$HOME/.config/systemd/user/switchbot-panel.service"
mkdir -p "$app" "$HOME/.config/systemd/user"
chmod 700 "$app"
# Preserve existing server credentials.
if [ ! -f "$app/switchbot-panel.env" ]; then
  install -m 600 switchbot-panel.env "$app/switchbot-panel.env"
fi
install -m 755 switchbot-panel "$app/switchbot-panel.new"
mv -f "$app/switchbot-panel.new" "$app/switchbot-panel"
if [ -f "$unit" ]; then
  cp -p "$unit" "$unit.backup-$(date +%Y%m%d%H%M%S)"
fi
cat > "$unit" <<'UNIT'
[Unit]
Description=SwitchBot Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/switchbot-panel
ExecStart=%h/switchbot-panel/switchbot-panel
Restart=on-failure
RestartSec=5s
UMask=0077

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemd-analyze --user verify "$unit"
# Keep the user manager running after the deployment SSH session closes.
if [ "$(loginctl show-user "$(id -un)" -p Linger --value)" != yes ]; then
  if ! loginctl enable-linger "$(id -un)"; then
    echo "Enable persistence on the server, then rerun deployment:" >&2
    echo "  sudo loginctl enable-linger $(id -un)" >&2
    exit 1
  fi
fi
if [ "$(loginctl show-user "$(id -un)" -p Linger --value)" != yes ]; then
  echo "Linger is not enabled; stopping deployment." >&2
  exit 1
fi
systemctl --user enable switchbot-panel.service
systemctl --user restart switchbot-panel.service
systemctl --user --no-pager status switchbot-panel.service
REMOTE
tar -C "$tmp" -cf - switchbot-panel switchbot-panel.env install.sh |
  remote 'set -eu; stage=$(mktemp -d); trap '\''rm -rf -- "$stage"'\'' EXIT; chmod 700 "$stage"; tar -xf - -C "$stage"; cd "$stage"; sh install.sh'
echo "Deployed to $target. Open http://$target:8080/ (default listen address)."
remote 'loginctl show-user "$(id -un)" -p Linger'
