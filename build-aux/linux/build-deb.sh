#!/usr/bin/env bash
# Builds a .deb from prebuilt binaries with dpkg-deb (no debhelper). Nothing
# is enabled or started at install; the maintainer scripts only create the
# coordinator's system account and restart its system service on upgrade if
# it runs, as dh_installsysusers and dh_installsystemd --no-enable would.
#
#   build-aux/linux/build-deb.sh <version> <amd64|arm64> <dir with swk and swk-tray> <output .deb>
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
maintainer=${DEB_MAINTAINER:-Switchworks Contributors <switchworks@example.invalid>}

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

doc="$stage/usr/share/doc/switchworks"
install -Dm644 "$root/README.md" "$doc/README.md"
install -Dm644 "$root/internal/web/assets/fonts/OFL-Onest.txt" "$doc/OFL-Onest.txt"
install -Dm644 "$root/internal/web/assets/fonts/OFL-JetBrainsMono.txt" "$doc/OFL-JetBrainsMono.txt"
cat > "$doc/copyright" <<'EOF'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: Switchworks
Source: https://github.com/jm2/switchworks

Files: *
Copyright: 2026 Switchworks Contributors
License: GPL-3.0-or-later
 On Debian systems, the full text of the GNU General Public License
 version 3 can be found in /usr/share/common-licenses/GPL-3.

Files: internal/web/assets/fonts/*
Copyright: 2021 The Onest Project Authors
           2020 The JetBrains Mono Project Authors
License: OFL-1.1
 The web UI built into /usr/bin/swk embeds the Onest and JetBrains Mono
 fonts. The full license texts are in /usr/share/doc/switchworks/OFL-*.txt.
EOF
chmod 0644 "$doc/copyright"
{
  printf 'switchworks (%s) unstable; urgency=medium\n\n' "$deb_version"
  printf '  * Release %s. See https://github.com/jm2/switchworks/releases\n\n' "$version"
  printf ' -- %s  %s\n' "$maintainer" "$(date -R -u -d "@${SOURCE_DATE_EPOCH:-$(date +%s)}")"
} | gzip -9n > "$doc/changelog.gz"
chmod 0644 "$doc/changelog.gz"
# Go binaries are statically linked by design (CGO_ENABLED=0).
install -Dm644 /dev/stdin "$stage/usr/share/lintian/overrides/switchworks" <<'EOF'
switchworks: statically-linked-binary [usr/bin/swk]
switchworks: statically-linked-binary [usr/bin/swk-tray]
switchworks: no-manual-page [usr/bin/swk]
switchworks: no-manual-page [usr/bin/swk-tray]
EOF

mkdir -p "$stage/DEBIAN"
installed_size=$(du -sk --exclude=DEBIAN "$stage" | cut -f1)
cat > "$stage/DEBIAN/control" <<EOF
Package: switchworks
Version: $deb_version
Architecture: $arch
Maintainer: $maintainer
Installed-Size: $installed_size
Section: devel
Priority: optional
Homepage: https://github.com/jm2/switchworks
Depends: systemd | systemd-sysusers | systemd-standalone-sysusers
Suggests: gnome-shell-extension-appindicator
Description: control plane for coding agents across your machines
 Switchworks shows every coding-agent session (Codex, Goose and other ACP
 agents) on every host in one place, from a browser, a phone or the swk
 command line, and lets agents on different machines message each other.
 .
 This package installs swk, the swk-tray status icon, systemd user units for
 the node and the coordinator, and a system unit that runs the coordinator
 as its own switchworks account. Nothing is enabled at install: each user
 runs "swk service install" to run the node as themselves, and "sudo swk
 service install --system coordinator" sets up the system coordinator.
EOF
cat > "$stage/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
# The account the coordinator's system service runs as.
if [ "$1" = configure ]; then
	systemd-sysusers ${DPKG_ROOT:+--root="$DPKG_ROOT"} switchworks.conf
fi
# Nothing is enabled; a coordinator that runs restarts on the new binary.
if [ "$1" = configure ] || [ "$1" = abort-upgrade ] || [ "$1" = abort-deconfigure ] || [ "$1" = abort-remove ]; then
	if [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ]; then
		systemctl --system daemon-reload >/dev/null || true
		if [ -n "$2" ]; then
			deb-systemd-invoke try-restart swk-coordinator.service >/dev/null || true
		fi
	fi
fi
exit 0
EOF
cat > "$stage/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
# Stop and disable the system coordinator when the package goes. Its
# account and /var/lib/switchworks (the CA and fleet state) are kept.
if [ "$1" = remove ]; then
	if [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ]; then
		deb-systemd-invoke stop swk-coordinator.service >/dev/null || true
	fi
	deb-systemd-helper disable swk-coordinator.service >/dev/null || true
fi
exit 0
EOF
cat > "$stage/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ] && [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ]; then
	systemctl --system daemon-reload >/dev/null || true
fi
exit 0
EOF
chmod 0755 "$stage/DEBIAN/postinst" "$stage/DEBIAN/prerm" "$stage/DEBIAN/postrm"
(cd "$stage" && find usr -type f -print0 | LC_ALL=C sort -z | xargs -0 md5sum) > "$stage/DEBIAN/md5sums"
chmod 0644 "$stage/DEBIAN/control" "$stage/DEBIAN/md5sums"

mkdir -p "$(dirname "$out")"
dpkg-deb --root-owner-group -Zxz --build "$stage" "$out"
dpkg-deb --info "$out"
