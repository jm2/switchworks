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
    # Git Bash's $! is not the Windows pid, and `taskkill /IM swk.exe`
    # would stop the coordinator's service too: stop the one swk.exe whose
    # command line names this run's node directory.
    cat >"$work/stop-node.ps1" <<'PS'
param([string]$Dir)
$p = @(Get-CimInstance Win32_Process -Filter "Name = 'swk.exe'" |
  Where-Object { $_.CommandLine -and $_.CommandLine.Contains($Dir) })
if ($p.Count -ne 1) { Write-Error "found $($p.Count) node processes for $Dir"; exit 1 }
Stop-Process -Id $p[0].ProcessId -Force
PS
    powershell -NoProfile -ExecutionPolicy Bypass -File "$(cygpath -w "$work/stop-node.ps1")" -Dir "$nodedir_win" ||
      echo "::warning title=system service smoke::could not stop the test node"
  else
    kill "$nodepid" 2>/dev/null || true
  fi
  # Bounded: a node that did not stop must not hang the job.
  for ((i = 0; i < 15; i++)); do
    kill -0 "$nodepid" 2>/dev/null || break
    sleep 1
  done
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
# Before init it does not start: not from swk, and not at boot.
if admin service start --system coordinator 2>"$work/start.err"; then
  fail "service start --system started a coordinator that is not set up"
fi
grep -q 'not set up' "$work/start.err" || fail "service start before init: $(cat "$work/start.err")"
if [ $os = linux ]; then
  # What boot would do: the unit's condition skips it, without a failure.
  ${as_admin[@]+"${as_admin[@]}"} systemctl start swk-coordinator.service
  [ "$(systemctl show --property=ConditionResult --value swk-coordinator.service)" = no ] ||
    fail "the unit ran without its config"
  [ "$(systemctl show --property=Result --value swk-coordinator.service)" = success ] ||
    fail "the skipped unit counts as failed"
else
  sc.exe qc swk-coordinator | grep -q DEMAND_START || fail "the service starts at boot before it is set up"
fi
admin --system coordinator init --host 127.0.0.1 || fail "swk --system coordinator init"
admin service start --system coordinator || fail "service start --system coordinator"
wait_for 60 "the coordinator to answer" admin --system coordinator health
if [ $os = windows ]; then
  sc.exe qc swk-coordinator | grep -q AUTO_START || fail "the started service does not start at boot"
fi

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
nodedir=$work/node-home/node
nodedir_win=$nodedir
[ $os = windows ] && nodedir_win=$(cygpath -w "$nodedir")
SWK_HOME="$work/node-home" "$swk" node run --dir "$nodedir_win" >"$work/node.log" 2>&1 &
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
