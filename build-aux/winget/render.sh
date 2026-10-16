#!/usr/bin/env bash
# Fills in the winget manifest templates for a release, in the directory
# layout winget-pkgs uses (manifests/j/jm2/Switchworks/<version>/):
#
#   build-aux/winget/render.sh <version> <installer sha256> <output dir> [release date]
#
# Submit the result to microsoft/winget-pkgs from a fork, by hand or with
# `wingetcreate submit <dir>`.
set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 ]]; then
  echo "usage: $0 <version> <installer sha256> <output dir> [YYYY-MM-DD]" >&2
  exit 2
fi
version=${1#v}
sha=${2^^}
out=$3
date=${4:-$(date -u +%Y-%m-%d)}
here=$(cd "$(dirname "$0")" && pwd)

if [[ ! $sha =~ ^[0-9A-F]{64}$ ]]; then
  echo "not a SHA-256: $2" >&2
  exit 2
fi
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  echo "not a release version: $1" >&2
  exit 2
fi

dir="$out/manifests/j/jm2/Switchworks/$version"
mkdir -p "$dir"
for f in jm2.Switchworks.yaml jm2.Switchworks.installer.yaml jm2.Switchworks.locale.en-US.yaml; do
  sed -e "s/@VERSION@/$version/g" -e "s/@SHA256@/$sha/g" -e "s/@RELEASE_DATE@/$date/g" \
    -e '/^# yaml-language-server/!{/^#/d}' "$here/$f" > "$dir/$f"
done
echo "$dir"
