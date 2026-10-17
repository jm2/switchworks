#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# End-to-end check of the coordinator's system service (Linux, or Windows
# under Git Bash from an elevated runner), for CI runners only: it installs
# the service with `swk service install --system coordinator`, initializes
# it with `swk --system`, checks it runs as its own account and keeps its
# files from this user, gives this user an operate token, enrolls a node
# that runs as this user, restarts the coordinator and uninstalls it.
#
#   build-aux/ci/system-service-smoke.sh <path to swk>
#
# On Linux it uses sudo unless it already runs as root. With a packaged
# swk (/usr/bin/swk from the .deb), the packaged unit is used.
set -euo pipefail

swk=$1
host=ci-system
case $(uname -s) in
  MINGW* | MSYS* | CYGWIN*) os=windows ;;
  *) os=linux ;;
esac
as_admin=()
if [ $os = linux ] && [ "$(id -u)" != 0 ]; then
  as_admin=(sudo)
fi
admin() { ${as_admin[@]+"${as_admin[@]}"} "$swk" "$@"; }

work=$(mktemp -d)
nodepid=
cleanup() {
  set +e
  stop_node
  rm -rf "$work"
}
trap cleanup EXIT

stop_node() {
  [ -n "$nodepid" ] || return 0
  if [ $os = windows ]; then
    # Git Bash has no /proc/<pid>/winpid: the node's status file has its
    # Windows pid.
    local winpid
    winpid=$(SWK_HOME="$work/node-home" "$swk" service status --json node | tr -d ' \r\n' | sed -n 's/.*"pid":\([0-9]*\).*/\1/p')
    [ -n "$winpid" ] && taskkill //F //PID "$winpid" >/dev/null 2>&1
  else
    kill "$nodepid" 2>/dev/null
  fi
  wait "$nodepid" 2>/dev/null
  nodepid=
}

fail() {
  echo "::error title=system service smoke::$*"
  dump_logs
  exit 1
}

dump_logs() {
  set +e
  # Log text is not ours: keep it from being read as workflow commands.
  local token
  token=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  echo "::stop-commands::$token"
  "$swk" service status
  if [ $os = linux ]; then
    ${as_admin[@]+"${as_admin[@]}"} journalctl -u swk-coordinator --no-pager -n 60
  else
    tail -n 60 "$PROGRAMDATA/Switchworks/coordinator/logs/coordinator.log"
  fi
  [ -f "$work/node.log" ] && tail -n 40 "$work/node.log"
  echo "::$token::"
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

"$swk" version
admin service install --system coordinator || fail "service install --system coordinator"
admin --system coordinator init --host 127.0.0.1 || fail "swk --system coordinator init"
admin service start --system coordinator || fail "service start --system coordinator"
wait_for 60 "the coordinator to answer" admin --system coordinator health

# It runs as its own account, never root, SYSTEM or this user. (The
# PowerShell code is single-quoted so that bash leaves its $ alone.)
# shellcheck disable=SC2016
if [ $os = linux ]; then
  pid=$(systemctl show --property=MainPID --value swk-coordinator.service)
  owner=$(ps -o user= -p "$pid" | tr -d ' ')
  [ "$owner" = switchworks ] || fail "the coordinator (pid $pid) runs as '$owner', not switchworks"
  perms=$(stat -c '%U %a' /var/lib/switchworks)
  [ "$perms" = "switchworks 700" ] || fail "/var/lib/switchworks is '$perms', not 'switchworks 700'"
  if [ "$(id -u)" != 0 ] && cat /var/lib/switchworks/coordinator/coordinator.json >/dev/null 2>&1; then
    fail "this user can read the system coordinator's config"
  fi
else
  pid=$(sc.exe queryex swk-coordinator | tr -d '\r' | sed -n 's/^ *PID *: *\([0-9]*\).*/\1/p')
  if [ -z "$pid" ] || [ "$pid" = 0 ]; then
    fail "sc.exe queryex shows no process for the coordinator"
  fi
  owner=$(powershell -NoProfile -Command "(Get-Process -Id $pid -IncludeUserName).UserName" | tr -d '\r')
  [ "$owner" = 'NT SERVICE\swk-coordinator' ] || fail "the coordinator runs as '$owner', not NT SERVICE\\swk-coordinator"
  # Only SYSTEM, administrators and the service's account may open its folder.
  while IFS= read -r id; do
    case $id in
      'NT AUTHORITY\SYSTEM' | 'BUILTIN\Administrators' | 'NT SERVICE\swk-coordinator') ;;
      *) fail "%ProgramData%\\Switchworks also lets '$id' in" ;;
    esac
  done < <(powershell -NoProfile -Command '(Get-Acl "$env:ProgramData\Switchworks").Access | ForEach-Object { $_.IdentityReference.Value }' | tr -d '\r')
fi
echo "the coordinator runs as $owner"

# This user's CLI, with an operate token from the coordinator's admin one.
admin --system token create ci-smoke --scope operate --cli-config >"$work/cli.json" || fail "token create"
export SWK_CLI_CONFIG=$work/cli.json
"$swk" hosts || fail "swk hosts with the operate token"

# A node running as this user, enrolled through the system coordinator.
token=$(admin --system coordinator enroll-token "$host") || fail "enroll-token"
echo "$token" | SWK_HOME="$work/node-home" "$swk" node enroll - || fail "node enroll"
SWK_HOME="$work/node-home" "$swk" node run >"$work/node.log" 2>&1 &
nodepid=$!
online() { "$swk" hosts | grep -E "^$host +online"; }
wait_for 60 "the node to come online" online

admin service restart --system coordinator || fail "service restart --system coordinator"
wait_for 60 "the coordinator to answer after a restart" admin --system coordinator health
wait_for 90 "the node to reconnect after a restart" online
stop_node

"$swk" service status coordinator | grep -q 'system service' || fail "service status does not show the system coordinator"
admin service uninstall --system coordinator || fail "service uninstall --system coordinator"
gone() { ! "$swk" service status --json coordinator | tr -d ' \r\n' | grep -q '"system":true'; }
wait_for 30 "the system service to be removed" gone
echo "system service smoke test passed"
