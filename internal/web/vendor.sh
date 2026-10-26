#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Fetches the web UI's third-party scripts into assets/vendor from the npm
# registry, each pinned to a version and checked against the registry's
# sha512 integrity before anything is unpacked. Run it after changing a
# version below, and commit the result:
#   bash internal/web/vendor.sh
set -euo pipefail
cd "$(dirname "$0")"
out=assets/vendor
tmp=$(mktemp -d -p "${TMPDIR:-/var/tmp}")
trap 'rm -rf "$tmp"' EXIT

# package version integrity file-in-package published-name license-file
pkgs=(
	"markdown-it 14.3.2 sha512-sHHjZ5fJKlgrG4qns2YwVcdNep35h5fERrfkD2YNsb9UFk0UIHarbiTaHKVMlPuWAoiilyK8Fv/jAm11slsY7Q== dist/markdown-it.min.js markdown-it.min.js LICENSE"
	"markdown-it-emoji 3.1.0 sha512-NhmMEH2ywduD4Nty1E8uB5NqfLhAT1VR0dyvoJyStKOqCzbZmVdn/+8wj7zpDsb/fLBikpCPsWwxqKlvMmbz4g== dist/markdown-it-emoji.min.js markdown-it-emoji.min.js LICENSE"
	"markdown-it-footnote 4.0.0 sha512-WYJ7urf+khJYl3DqofQpYfEYkZKbmXmwxQV8c8mO/hGIhgZ1wOe7R4HLFNwqx7TjILbnC98fuyeSsin19JdFcQ== dist/markdown-it-footnote.min.js markdown-it-footnote.min.js LICENSE"
	"dompurify 3.4.16 sha512-sqo+pNp3qRhCIpbgRi1y8Tgk27Bo2Ry7w0dC1NBeNTdZChWjz9Xb/KOoZbRP/R6pQZ80Qw8YhXw13hWWBbMRnQ== dist/purify.min.js purify.min.js LICENSE"
	"@highlightjs/cdn-assets 11.12.0 sha512-KvOKXODaiFmId9xaq3xc5xCL66wVLUuOngDbO9B/kewbFTqdGbn2nJxNhN3H5R1cgDTVj6R8vH0zgiNDEGjpDw== highlight.min.js highlight.min.js LICENSE"
	"katex 0.19.0 sha512-v6Tznz3zJ7u3niRCoDTsumM2+HA2XXcCu+WAacCeHD2z3p9A9Ks987o5FzfTGBN0e8A0vjEgIDvLbICrXpdw/Q== dist/katex.min.js katex.min.js LICENSE"
	"mermaid 12.1.0 sha512-wlVCp+8eTupfCeeFvoZNNiTuHrvag0P2jz/ILgb/f/6jkVokUefOcujefi8qUe/j2asHiSePncVsz/xzzA80LQ== dist/mermaid.min.js mermaid.min.js LICENSE"
)

rm -rf "$out"
mkdir -p "$out/licenses"
{
	echo "# Vendored scripts"
	echo
	echo "Fetched by \`internal/web/vendor.sh\` from the npm registry and checked against its"
	echo "sha512 integrity. Do not edit; change the versions in the script and run it again."
	echo
	echo "| Package | Version | License | File |"
	echo "| --- | --- | --- | --- |"
} > "$out/README.md"
for p in "${pkgs[@]}"; do
	read -r name ver integ src dst lic <<<"$p"
	base=${name##*/}
	tgz="$tmp/$base-$ver.tgz"
	curl -fsSL -o "$tgz" "https://registry.npmjs.org/$name/-/$base-$ver.tgz"
	got="sha512-$(openssl dgst -sha512 -binary "$tgz" | base64 -w0)"
	if [ "$got" != "$integ" ]; then
		echo "$name $ver: integrity $got, want $integ" >&2
		exit 1
	fi
	mkdir "$tmp/$base"
	tar -xzf "$tgz" -C "$tmp/$base" --no-same-owner --no-same-permissions "package/$src" "package/$lic"
	install -m 0644 "$tmp/$base/package/$src" "$out/$dst"
	install -m 0644 "$tmp/$base/package/$lic" "$out/licenses/$base.txt"
	spdx=$(tar -xzOf "$tgz" package/package.json | python3 -I -c 'import json,sys; print(json.load(sys.stdin).get("license", "?"))')
	echo "| $name | $ver | $spdx | \`$dst\` |" >> "$out/README.md"
done
echo "vendored into $out"
