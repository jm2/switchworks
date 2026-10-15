#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# End-to-end check of the user services on this machine (Linux, macOS or
# Windows under Git Bash), for CI runners only: it initializes a coordinator
# and enrolls a node in the real default directories, runs both through the
# platform's service manager (systemd user units, LaunchAgents, logon
# tasks), checks they run as this user and connect, restarts and stops
# them, and uninstalls them.
#
#   build-aux/ci/service-smoke.sh <path to swyd>
#
# Failures are printed as GitHub Actions error annotations.
set -euo pipefail

swyd=$1
user=$(id -un)
host=ci-smoke

fail() {
  echo "::error title=service smoke::$*"
  dump_logs
  exit 1
}

dump_logs() {
  set +e
  # Log text is not ours: keep it from being read as workflow commands.
  local token
  token=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  echo "::stop-commands::$token"
  "$swyd" service status || true
  case $(uname -s) in
    Linux)
      for u in swyd-coordinator swyd-node; do
        echo "--- journal: $u"
        journalctl --user -u "$u" --no-pager -n 40
      done ;;
  esac
  local dirs=() d
  for d in "$("$swyd" service status --json node | sed -n 's/.*"log_dir": "\(.*\)".*/\1/p')" \
           "$("$swyd" service status --json coordinator | sed -n 's/.*"log_dir": "\(.*\)".*/\1/p')"; do
    d=${d//\\\\/\\}
    [ -d "$d" ] && dirs+=("$d")
  done
  for d in ${dirs[@]+"${dirs[@]}"}; do
    for f in "$d"/*.log*; do
      [ -f "$f" ] || continue
      echo "--- $f"
      tail -n 40 "$f"
    done
  done
  echo "::$token::"
  # The last lines as annotations, readable without the job log (escaped,
  # so they cannot form a workflow command).
  for d in ${dirs[@]+"${dirs[@]}"}; do
    for f in "$d"/*.log*; do
      [ -f "$f" ] || continue
      echo "::error title=$(basename "$f")::$(tail -n 15 "$f" | sed -e 's/%/%25/g' -e 's/\r/%0D/g' | awk '{printf "%s%%0A", $0}')"
    done
  done
}

# wait_for <seconds> <description> <command...>
wait_for() {
  local secs=$1 what=$2
  shift 2
  for ((i = 0; i < secs; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timed out after ${secs}s waiting for: $what"
}

status_has() { "$swyd" service status --json "$1" | tr -d ' \r\n' | grep -q "$2"; }

pid_of() { "$swyd" service status --json "$1" | tr -d ' \r\n' | sed -n 's/.*"pid":\([0-9]*\).*/\1/p'; }

# The process must run as this user: never root, SYSTEM or elevated.
check_owner() {
  local pid owner
  pid=$(pid_of "$1")
  [ -n "$pid" ] || fail "$1: no pid in its status"
  case $(uname -s) in
    MINGW* | MSYS* | CYGWIN*)
      owner=$(powershell -NoProfile -Command "(Get-Process -Id $pid -IncludeUserName).UserName" | tr -d '\r')
      owner=${owner##*\\}
      [ "${owner,,}" = "${USERNAME,,}" ] || fail "$1 (pid $pid) runs as '$owner', not $USERNAME"
      echo "$1 (pid $pid) runs as $owner" ;;
    *)
      owner=$(ps -o user= -p "$pid" | tr -d ' ')
      [ "$owner" = "$user" ] || fail "$1 (pid $pid) runs as '$owner', not $user"
      echo "$1 (pid $pid) runs as $owner" ;;
  esac
}

if [ "$(uname -s)" = Linux ]; then
  # A runner has no login session: start this user's systemd manager.
  sudo loginctl enable-linger "$user"
  XDG_RUNTIME_DIR=/run/user/$(id -u)
  export XDG_RUNTIME_DIR
  wait_for 30 "the systemd user manager" test -S "$XDG_RUNTIME_DIR/bus"
  export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
  wait_for 30 "systemctl --user" systemctl --user is-system-running --wait
fi

"$swyd" version
"$swyd" coordinator init --host 127.0.0.1 || fail "coordinator init"
"$swyd" service install coordinator || fail "service install coordinator"
# "running" can come from the service manager before the process has
# written its status file; wait for the file (it carries the pid).
wait_for 60 "the coordinator to run" status_has coordinator '"pid":[1-9]'
check_owner coordinator

token=$("$swyd" coordinator enroll-token "$host") || fail "enroll-token"
"$swyd" node enroll "$token" || fail "node enroll"
"$swyd" service install node || fail "service install node"
wait_for 90 "the node to connect" status_has node '"connected":true'
check_owner node
wait_for 30 "the coordinator to list the host as online" sh -c "\"$swyd\" hosts | grep -E '^$host +online'"

"$swyd" service restart node || fail "service restart node"
wait_for 90 "the node to reconnect after a restart" status_has node '"connected":true'

"$swyd" service stop node || fail "service stop node"
wait_for 30 "the node to stop" sh -c "! \"$swyd\" service status --json node | tr -d ' \r\n' | grep -q '\"running\":true'"
"$swyd" service start node || fail "service start node"
wait_for 90 "the node to connect after a start" status_has node '"connected":true'

"$swyd" service uninstall node || fail "service uninstall node"
"$swyd" service uninstall coordinator || fail "service uninstall coordinator"
status_has node '"installed":false' || fail "the node service is still installed"
status_has coordinator '"installed":false' || fail "the coordinator service is still installed"
wait_for 30 "both to stop" sh -c "! \"$swyd\" service status --json | tr -d ' \r\n' | grep -q '\"running\":true'"
echo "service smoke test passed"
