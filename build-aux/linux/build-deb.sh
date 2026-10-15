#!/usr/bin/env bash
# Builds a .deb from prebuilt binaries with dpkg-deb (no debhelper: nothing is
# enabled or started at install, and there are no maintainer scripts).
#
#   build-aux/linux/build-deb.sh <version> <amd64|arm64> <dir with swyd and swyd-tray> <output .deb>
#
# DEB_MAINTAINER sets the Maintainer field.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 <version> <amd64|arm64> <bindir> <output.deb>" >&2
  exit 2
fi
version=${1#v}
arch=$2
bindir=$3
out=$4
root=$(cd "$(dirname "$0")/../.." && pwd)
maintainer=${DEB_MAINTAINER:-Switchyard Contributors <switchyard@example.invalid>}

case $arch in
  amd64 | arm64) ;;
  *) echo "unsupported architecture $arch" >&2; exit 2 ;;
esac
# Debian sorts "~" before anything, so 1.0.0~rc.1 precedes 1.0.0.
deb_version=${version//-/\~}
if [[ ! $deb_version =~ ^[0-9][A-Za-z0-9.+~]*$ ]]; then
  echo "version $version is not a valid Debian version" >&2
  exit 2
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
chmod 0755 "$stage"
"$root/build-aux/linux/install-tree.sh" "$stage" "$bindir" /usr

doc="$stage/usr/share/doc/switchyard"
install -Dm644 "$root/README.md" "$doc/README.md"
install -Dm644 "$root/internal/web/assets/fonts/OFL-Onest.txt" "$doc/OFL-Onest.txt"
install -Dm644 "$root/internal/web/assets/fonts/OFL-JetBrainsMono.txt" "$doc/OFL-JetBrainsMono.txt"
cat > "$doc/copyright" <<'EOF'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: Switchyard
Source: https://github.com/jm2/switchyard

Files: *
Copyright: 2026 Switchyard Contributors
License: GPL-3.0-or-later
 On Debian systems, the full text of the GNU General Public License
 version 3 can be found in /usr/share/common-licenses/GPL-3.

Files: internal/web/assets/fonts/*
Copyright: 2021 The Onest Project Authors
           2020 The JetBrains Mono Project Authors
License: OFL-1.1
 The web UI built into /usr/bin/swyd embeds the Onest and JetBrains Mono
 fonts. The full license texts are in /usr/share/doc/switchyard/OFL-*.txt.
EOF
chmod 0644 "$doc/copyright"
{
  printf 'switchyard (%s) unstable; urgency=medium\n\n' "$deb_version"
  printf '  * Release %s. See https://github.com/jm2/switchyard/releases\n\n' "$version"
  printf ' -- %s  %s\n' "$maintainer" "$(date -R -u -d "@${SOURCE_DATE_EPOCH:-$(date +%s)}")"
} | gzip -9n > "$doc/changelog.gz"
chmod 0644 "$doc/changelog.gz"
# Go binaries are statically linked by design (CGO_ENABLED=0).
install -Dm644 /dev/stdin "$stage/usr/share/lintian/overrides/switchyard" <<'EOF'
switchyard: statically-linked-binary [usr/bin/swyd]
switchyard: statically-linked-binary [usr/bin/swyd-tray]
switchyard: no-manual-page [usr/bin/swyd]
switchyard: no-manual-page [usr/bin/swyd-tray]
EOF

mkdir -p "$stage/DEBIAN"
installed_size=$(du -sk --exclude=DEBIAN "$stage" | cut -f1)
cat > "$stage/DEBIAN/control" <<EOF
Package: switchyard
Version: $deb_version
Architecture: $arch
Maintainer: $maintainer
Installed-Size: $installed_size
Section: devel
Priority: optional
Homepage: https://github.com/jm2/switchyard
Suggests: gnome-shell-extension-appindicator
Description: control plane for coding agents across your machines
 Switchyard shows every coding-agent session (Codex, Goose and other ACP
 agents) on every host in one place, from a browser, a phone or the swyd
 command line, and lets agents on different machines message each other.
 .
 This package installs swyd, the swyd-tray status icon, and systemd user
 units for the node and the coordinator. Nothing is enabled at install: each
 user runs "swyd service install" (or "systemctl --user enable --now
 swyd-node") to run the node as themselves.
EOF
(cd "$stage" && find usr -type f -print0 | LC_ALL=C sort -z | xargs -0 md5sum) > "$stage/DEBIAN/md5sums"
chmod 0644 "$stage/DEBIAN/control" "$stage/DEBIAN/md5sums"

mkdir -p "$(dirname "$out")"
dpkg-deb --root-owner-group -Zxz --build "$stage" "$out"
dpkg-deb --info "$out"
