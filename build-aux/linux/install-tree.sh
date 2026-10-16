#!/bin/sh
# Installs Switchworks's files into a package staging tree. Every package
# (rpm, deb, Arch) uses this, so they ship the same files:
#
#   <prefix>/bin/swk, <prefix>/bin/swk-tray
#   <prefix>/lib/systemd/user/swk-node.service, swk-coordinator.service
#   <prefix>/share/applications/io.github.jm2.Switchworks.desktop (the tray)
#   <prefix>/share/icons/hicolor/*/apps/io.github.jm2.Switchworks.{png,svg}
#
# The units are *user* units. Nothing here enables them, creates a system
# service or a system user: each person turns the node on for themselves
# with `swk service install` (or `systemctl --user enable --now swk-node`).
#
#   build-aux/linux/install-tree.sh <destdir> <dir with swk and swk-tray> [prefix]
set -eu

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <destdir> <bindir> [prefix]" >&2
  exit 2
fi
dest=$1
bindir=$2
prefix=${3:-/usr}
root=$(cd "$(dirname "$0")/../.." && pwd)
app=io.github.jm2.Switchworks

install -Dm755 "$bindir/swk" "$dest$prefix/bin/swk"
install -Dm755 "$bindir/swk-tray" "$dest$prefix/bin/swk-tray"

for role in node coordinator; do
  unit="$dest$prefix/lib/systemd/user/swk-$role.service"
  mkdir -p "$(dirname "$unit")"
  sed "s|@SWK@|$prefix/bin/swk|" "$root/internal/service/units/swk-$role.service" > "$unit"
  chmod 0644 "$unit"
done

install -Dm644 "$root/data/$app.desktop" "$dest$prefix/share/applications/$app.desktop"
for icon in "$root"/data/icons/hicolor/*/apps/"$app".*; do
  rel=${icon#"$root/data/icons/"}
  install -Dm644 "$icon" "$dest$prefix/share/icons/$rel"
done
