# Builds swyd and swyd-tray from the release source. Used by COPR (through
# Packit, see .packit.yaml) and by the release workflow's .rpm job.
#
# go.mod asks for Go 1.27.2. When the builder's Go is older, the go command
# downloads that toolchain from proxy.golang.org (checked against
# sum.golang.org), so COPR builds need network access (enable_net).

# Static, stripped Go binaries: there is no debug information to package.
%global debug_package %{nil}
# The version stamped into the binaries: the release tag. The release
# workflow passes it for pre-releases, whose RPM version uses "~".
%{!?swyd_tag:%global swyd_tag v%{version}}

Name:           switchyard
# Packit replaces Version with the release tag minus its "v" (see .packit.yaml).
Version:        0.1.0
Release:        1%{?dist}
Summary:        Control plane for coding agents across your machines

License:        GPL-3.0-or-later AND OFL-1.1
URL:            https://github.com/jm2/switchyard
Source0:        https://github.com/jm2/switchyard/archive/refs/tags/v%{version}.tar.gz

ExclusiveArch:  x86_64 aarch64

BuildRequires:  golang >= 1.21
BuildRequires:  make
BuildRequires:  systemd-rpm-macros
BuildRequires:  desktop-file-utils

# The tray icon needs a StatusNotifierItem host; GNOME has one through the
# AppIndicator extension.
Suggests:       (gnome-shell-extension-appindicator if gnome-shell)

%description
Switchyard shows every coding-agent session (Codex, Goose and other ACP
agents) on every host in one place, from a browser, a phone or the swyd
command line, and lets agents on different machines message each other.

This package installs swyd, the swyd-tray status icon, and systemd user units
for the node and the coordinator. Nothing is enabled at install: each user
runs "swyd service install" (or "systemctl --user enable --now swyd-node") to
run the node as themselves.

%prep
%autosetup -p1 -n %{name}-%{version}

%build
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
export GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.org}"
export GOFLAGS="${GOFLAGS:-} -mod=readonly"
make build tray VERSION=%{swyd_tag}

%install
build-aux/linux/install-tree.sh %{buildroot} . %{_prefix}

%check
desktop-file-validate %{buildroot}%{_datadir}/applications/io.github.jm2.Switchyard.desktop
./swyd version | grep -F "%{swyd_tag} "

# No %%systemd_user_post: it would enable the units for every user by preset.
# Each user enables the node for themselves.

%files
%license LICENSE internal/web/assets/fonts/OFL-Onest.txt internal/web/assets/fonts/OFL-JetBrainsMono.txt
%doc README.md
%{_bindir}/swyd
%{_bindir}/swyd-tray
%{_userunitdir}/swyd-node.service
%{_userunitdir}/swyd-coordinator.service
%{_datadir}/applications/io.github.jm2.Switchyard.desktop
%{_datadir}/icons/hicolor/*/apps/io.github.jm2.Switchyard.png
%{_datadir}/icons/hicolor/scalable/apps/io.github.jm2.Switchyard.svg

%changelog
* Thu Oct 15 2026 Switchyard Contributors - 0.1.0-1
- Initial package: swyd, swyd-tray, systemd user units.
