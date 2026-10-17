# Builds swk and swk-tray from the release source. Used by COPR (through
# Packit, see .packit.yaml) and by the release workflow's .rpm job.
#
# go.mod asks for Go 1.27.2. When the builder's Go is older, the go command
# downloads that toolchain from proxy.golang.org (checked against
# sum.golang.org), so COPR builds need network access (enable_net).

# Static, stripped Go binaries: there is no debug information to package.
%global debug_package %{nil}
# The version stamped into the binaries: the release tag. The release
# workflow passes it for pre-releases, whose RPM version uses "~".
%{!?swk_tag:%global swk_tag v%{version}}

Name:           switchworks
# Packit replaces Version with the release tag minus its "v" (see .packit.yaml).
Version:        0.1.0
Release:        1%{?dist}
Summary:        Control plane for coding agents across your machines

License:        GPL-3.0-or-later AND OFL-1.1
URL:            https://github.com/jm2/switchworks
Source0:        https://github.com/jm2/switchworks/archive/refs/tags/v%{version}.tar.gz

ExclusiveArch:  x86_64 aarch64

BuildRequires:  golang >= 1.21
BuildRequires:  make
BuildRequires:  systemd-rpm-macros
BuildRequires:  desktop-file-utils
# The coordinator's system service and its account.
%{?systemd_requires}
Requires(pre):  systemd

# The tray icon needs a StatusNotifierItem host; GNOME has one through the
# AppIndicator extension.
Suggests:       (gnome-shell-extension-appindicator if gnome-shell)

%description
Switchworks shows every coding-agent session (Codex, Goose and other ACP
agents) on every host in one place, from a browser, a phone or the swk
command line, and lets agents on different machines message each other.

This package installs swk, the swk-tray status icon, systemd user units for
the node and the coordinator, and a system unit that runs the coordinator as
its own switchworks account. Nothing is enabled at install: each user runs
"swk service install" to run the node as themselves, and "sudo swk service
install --system coordinator" sets up the system coordinator.

%prep
%autosetup -p1 -n %{name}-%{version}

%build
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
export GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.org}"
export GOFLAGS="${GOFLAGS:-} -mod=readonly"
make build tray VERSION=%{swk_tag}

%install
build-aux/linux/install-tree.sh %{buildroot} . %{_prefix}

%check
desktop-file-validate %{buildroot}%{_datadir}/applications/io.github.jm2.Switchworks.desktop
./swk version | grep -F "%{swk_tag} "

# No %%systemd_user_post: it would enable the units for every user by preset.
# Each user enables the node for themselves.

# %%pre creates the system coordinator's account from the sysusers.d line
# this package ships (what %%sysusers_create_compat expands to; newer rpm
# also creates it from the file itself). The system unit is not enabled by
# preset (Fedora's presets leave it off); it is restarted on upgrade if it
# runs, and stopped and disabled on removal.
%pre
systemd-sysusers --replace=%{_sysusersdir}/switchworks.conf - <<'EOF' || :
u switchworks - "Switchworks coordinator" /var/lib/switchworks
EOF

%post
%systemd_post swk-coordinator.service

%preun
%systemd_preun swk-coordinator.service

%postun
%systemd_postun_with_restart swk-coordinator.service

%files
%license LICENSE internal/web/assets/fonts/OFL-Onest.txt internal/web/assets/fonts/OFL-JetBrainsMono.txt
%doc README.md
%{_bindir}/swk
%{_bindir}/swk-tray
%{_userunitdir}/swk-node.service
%{_userunitdir}/swk-coordinator.service
%{_unitdir}/swk-coordinator.service
%{_sysusersdir}/switchworks.conf
%{_datadir}/applications/io.github.jm2.Switchworks.desktop
%{_datadir}/icons/hicolor/*/apps/io.github.jm2.Switchworks.png
%{_datadir}/icons/hicolor/scalable/apps/io.github.jm2.Switchworks.svg

%changelog
* Thu Oct 15 2026 Switchworks Contributors - 0.1.0-1
- Initial package: swk, swk-tray, systemd user units, and a system unit
  that runs the coordinator as its own switchworks account.
