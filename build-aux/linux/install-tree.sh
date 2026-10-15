#!/bin/sh
# Installs Switchyard's files into a package staging tree. Every package
# (rpm, deb, Arch) uses this, so they ship the same files:
#
#   <prefix>/bin/swyd, <prefix>/bin/swyd-tray
#   <prefix>/lib/systemd/user/swyd-node.service, swyd-coordinator.service
#   <prefix>/share/applications/io.github.jm2.Switchyard.desktop (the tray)
#   <prefix>/share/icons/hicolor/*/apps/io.github.jm2.Switchyard.{png,svg}
#
# The units are *user* units. Nothing here enables them, creates a system
# service or a system user: each person turns the node on for themselves
# with `swyd service install` (or `systemctl --user enable --now swyd-node`).
#
#   build-aux/linux/install-tree.sh <destdir> <dir with swyd and swyd-tray> [prefix]
set -eu

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <destdir> <bindir> [prefix]" >&2
  exit 2
fi
dest=$1
bindir=$2
prefix=${3:-/usr}
root=$(cd "$(dirname "$0")/../.." && pwd)
app=io.github.jm2.Switchyard

install -Dm755 "$bindir/swyd" "$dest$prefix/bin/swyd"
install -Dm755 "$bindir/swyd-tray" "$dest$prefix/bin/swyd-tray"

for role in node coordinator; do
  unit="$dest$prefix/lib/systemd/user/swyd-$role.service"
  mkdir -p "$(dirname "$unit")"
  sed "s|@SWYD@|$prefix/bin/swyd|" "$root/internal/service/units/swyd-$role.service" > "$unit"
  chmod 0644 "$unit"
done

install -Dm644 "$root/data/$app.desktop" "$dest$prefix/share/applications/$app.desktop"
for icon in "$root"/data/icons/hicolor/*/apps/"$app".*; do
  rel=${icon#"$root/data/icons/"}
  install -Dm644 "$icon" "$dest$prefix/share/icons/$rel"
done
