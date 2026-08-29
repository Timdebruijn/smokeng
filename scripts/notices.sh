#!/bin/sh
# Collect the licences of every module actually linked into the binary.
#
# smokeng ships as one static file, so its dependencies travel inside it.
# Most are MIT or BSD and require their copyright notice to be distributed
# with the binary; Apache-2.0 additionally requires the licence text. This
# produces that file straight from the module cache, with no extra tooling:
# `go list -deps` names exactly what was linked, rather than everything that
# happens to be in go.mod.
set -eu

out=${1:-THIRD-PARTY-NOTICES}
{
	echo "Third-party licences bundled into the smokeng binary."
	echo
	echo "smokeng itself is MIT licensed; see LICENSE. The modules below are"
	echo "statically linked into the released binaries, and their licences are"
	echo "reproduced here as those licences require."
} >"$out"

count=0
go list -deps -f '{{if .Module}}{{.Module.Path}}	{{.Module.Dir}}{{end}}' ./cmd/smokeng |
	sort -u >/tmp/smokeng-modules.$$

while IFS='	' read -r path dir; do
	[ -n "${dir:-}" ] || continue
	licence=$(ls "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* 2>/dev/null | head -1 || true)
	[ -n "$licence" ] || continue
	{
		echo
		echo "================================================================"
		echo "$path"
		echo "================================================================"
		cat "$licence"
	} >>"$out"
	count=$((count + 1))
done </tmp/smokeng-modules.$$
rm -f /tmp/smokeng-modules.$$

echo "wrote $out ($count modules)"
