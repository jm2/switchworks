#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# End-to-end check of the coordinator's container image under rootless
# Podman and Quadlet, for CI runners only: it builds the image, initializes
# a coordinator in a volume, runs it as a Quadlet systemd user unit, checks
# its healthcheck, enrolls a node running on this machine through
# `podman exec`, restarts the unit (the state survives in the volume) and
# removes everything.
#
#   build-aux/ci/container-smoke.sh <path to a linux swyd for this arch>
#
# Run from the repository root. Failures are printed as GitHub Actions
# error annotations.
set -euo pipefail

swyd=$1
arch=$(go env GOARCH 2>/dev/null || uname -m)
case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
image=localhost/switchyard-coordinator:ci
unit=switchyard-coordinator
volume=swyd-coordinator
quadlets=${XDG_CONFIG_HOME:-$HOME/.config}/containers/systemd
work=$(mktemp -d)
export SWYD_HOME=$work/node-home

fail() {
  echo "::error title=container smoke::$*"
  set +e
  local token
  token=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  echo "::stop-commands::$token"
  systemctl --user status "$unit" --no-pager
  journalctl --user -u "$unit" --no-pager -n 60
  podman ps -a
  [ -f "$work/node.log" ] && tail -n 40 "$work/node.log"
  echo "::$token::"
  exit 1
}

cleanup() {
  set +e
  [ -n "${node_pid:-}" ] && kill "$node_pid" 2>/dev/null
  systemctl --user stop "$unit" 2>/dev/null
  rm -f "$quadlets/$unit.container"
  systemctl --user daemon-reload 2>/dev/null
  podman rm -f "$unit" >/dev/null 2>&1
  podman volume rm -f "$volume" >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT

[ "$(id -u)" != 0 ] || fail "run this as an ordinary user: the point is rootless Podman"
command -v podman >/dev/null || fail "podman is not installed"

echo "--- build the image"
mkdir -p dist
cp "$swyd" "dist/swyd-linux-$arch"
podman build -q -f build-aux/container/Containerfile --build-arg TARGETARCH="$arch" \
  --build-arg VERSION=ci -t "$image" . || fail "image build failed"
size=$(podman image inspect "$image" --format '{{.Size}}')
echo "image size: $size bytes"
[ "$size" -lt 40000000 ] || fail "the image is $size bytes; it should be little more than swyd"
user=$(podman image inspect "$image" --format '{{.Config.User}}')
[ "$user" = 65532:65532 ] || fail "the image runs as $user, not 65532:65532"

echo "--- initialize the coordinator in a volume"
podman volume rm -f "$volume" >/dev/null 2>&1 || true
podman run --rm -v "$volume:/data" "$image" coordinator init --host localhost,127.0.0.1 \
  || fail "coordinator init in the container failed"

echo "--- run it as a Quadlet user unit"
mkdir -p "$quadlets"
# The CI image is local: never pull it, and skip auto-update.
sed -e "s|^Image=.*|Image=$image|" -e '/^AutoUpdate=/d' \
  build-aux/container/switchyard-coordinator.container > "$quadlets/$unit.container"
if ! podman info --format '{{.Host.Pasta.Executable}}' 2>/dev/null | grep -q .; then
  # Older Podman without pasta: slirp4netns still works, minus real
  # client addresses.
  sed -i '/^Network=pasta/d' "$quadlets/$unit.container"
fi
systemctl --user daemon-reload
systemctl --user start "$unit" || fail "the unit did not start"

healthy() { podman exec "$unit" swyd coordinator health >/dev/null 2>&1; }
for _ in $(seq 60); do healthy && break; sleep 1; done
healthy || fail "the coordinator never became healthy"
podman healthcheck run "$unit" || fail "podman's healthcheck failed"
# Rootless, with USER 65532 in the image: on the host the process is one
# of this user's subordinate ids, never root.
owner=$(podman top "$unit" huser | tail -n 1)
case $owner in 0|root) fail "the coordinator runs as root on the host" ;; esac
echo "host user of the coordinator process: $owner"

echo "--- enroll a node on this machine"
podman exec "$unit" swyd coordinator enroll-token ci-node | "$swyd" node enroll - \
  || fail "enrolling a node failed"
"$swyd" node run > "$work/node.log" 2>&1 &
node_pid=$!
online() { podman exec "$unit" swyd hosts | grep -q '^ci-node *online'; }
for _ in $(seq 30); do online && break; sleep 1; done
online || fail "the node never came online"

echo "--- a scoped token works from outside the container"
podman exec "$unit" swyd token create ci-cli --scope read --cli-config > "$work/cli.json" \
  || fail "making a token failed"
SWYD_CLI_CONFIG=$work/cli.json "$swyd" hosts | grep -q ci-node || fail "the read token could not list hosts"

echo "--- restart: the state survives in the volume"
systemctl --user restart "$unit"
for _ in $(seq 60); do healthy && break; sleep 1; done
healthy || fail "the coordinator did not come back after a restart"
for _ in $(seq 60); do online && break; sleep 1; done
online || fail "the node did not reconnect after a restart"

echo "container smoke: ok"
