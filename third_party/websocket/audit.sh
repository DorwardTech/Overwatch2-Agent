#!/usr/bin/env bash
# Audits the patched copy of gorilla/websocket in this directory. CI runs it in
# the Dependency audits / vulnerability scan jobs, next to govulncheck.
#
# Why this is needed: agent/go.mod (and simulator/go.mod) replace
# github.com/gorilla/websocket with this directory. govulncheck looks modules up
# by their replacement path, so with the replace in place it never asks the
# vulnerability database about gorilla/websocket at all. It would stay clean even
# if this copy lost its patch, or if a new advisory came out against v1.5.3. So:
#
#   1. The copy must be exactly upstream v1.5.3's package files plus
#      overwatch.patch (the mask-key fix and doc.go's gofmt). Anything else fails.
#   2. govulncheck runs on a scratch copy of each module with the replace
#      removed, so it sees v1.5.3 as published. It must find GO-2026-6278 (the
#      advisory this copy fixes) and nothing else. Any other advisory fails, as
#      it would have before the replace.
#   3. If upstream has tagged a release after v1.5.3, it warns: that is the
#      signal to test it and drop this copy (OVERWATCH.md, "When to remove it").
#
# Usage: audit.sh MODULE_DIR...   (e.g. from the monorepo root: agent simulator;
# in the public agent repo: .)  Needs go, govulncheck on PATH, patch and network.
set -euo pipefail

MODULE=github.com/gorilla/websocket
VERSION=v1.5.3
FIXED_ADVISORY=GO-2026-6278
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

[ "$#" -gt 0 ] || { echo "usage: $0 MODULE_DIR..." >&2; exit 2; }

echo "== 1. This copy is $MODULE $VERSION plus overwatch.patch"
upstream=$(cd "$work" && go mod download -json "$MODULE@$VERSION" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -d "$upstream" ] || { echo "could not download $MODULE@$VERSION" >&2; exit 1; }
expect="$work/expect"
mkdir -p "$expect"
for f in "$upstream"/*.go "$upstream"/LICENSE "$upstream"/AUTHORS "$upstream"/go.mod; do
    case "$f" in *_test.go) continue ;; esac
    cp "$f" "$expect/"
done
chmod -R u+w "$expect"
(cd "$expect" && patch -p1 -s --no-backup-if-mismatch < "$here/overwatch.patch")
if diff -r -x OVERWATCH.md -x overwatch.patch -x audit.sh "$expect" "$here"; then
    echo "ok: identical"
else
    echo "FAIL: the copy differs from $VERSION plus overwatch.patch (above). Change the copy only by" >&2
    echo "      updating overwatch.patch to match, and say why in OVERWATCH.md." >&2
    exit 1
fi

echo "== 2. govulncheck with the replace removed"
status=0
for dir in "$@"; do
    name=$(basename "$(cd "$dir" && pwd)")
    scratch="$work/mod-$name"
    cp -R "$dir" "$scratch"
    (
        cd "$scratch"
        go mod edit -dropreplace "$MODULE" -dropreplace "$MODULE@$VERSION"
        go mod tidy
    ) >/dev/null 2>&1 || { echo "FAIL: $dir: could not drop the replace" >&2; exit 1; }
    # The scan only means something if it sees the published module at the
    # version the copy is based on.
    resolved=$(cd "$scratch" && go list -m -f '{{.Version}}{{if .Replace}} (still replaced){{end}}' "$MODULE" 2>&1) || true
    if [ "$resolved" != "$VERSION" ]; then
        echo "FAIL: $dir: with the replace dropped, $MODULE resolves to '$resolved', not $VERSION;" >&2
        echo "      update VERSION and overwatch.patch here, or the go.mod, so they agree." >&2
        exit 1
    fi
    out=$(cd "$scratch" && govulncheck ./... 2>&1) || true
    ids=$(printf '%s\n' "$out" | sed -n 's/^Vulnerability #[0-9]*: \(GO-[0-9]*-[0-9]*\).*/\1/p' | sort -u)
    others=$(printf '%s\n' "$ids" | grep -v -x "$FIXED_ADVISORY" | grep . || true)
    if [ -n "$others" ]; then
        echo "FAIL: $dir: advisories other than $FIXED_ADVISORY: $(echo $others)" >&2
        printf '%s\n' "$out" >&2
        status=1
    elif ! printf '%s\n' "$ids" | grep -q -x "$FIXED_ADVISORY"; then
        if printf '%s\n' "$out" | grep -q 'No vulnerabilities found'; then
            echo "note: $dir: $FIXED_ADVISORY is no longer reported against $VERSION; check whether the advisory changed."
        else
            echo "FAIL: $dir: govulncheck did not run cleanly:" >&2
            printf '%s\n' "$out" >&2
            status=1
        fi
    else
        echo "ok: $dir: only $FIXED_ADVISORY, which this copy fixes"
    fi
done
[ "$status" -eq 0 ] || exit 1

echo "== 3. Upstream releases after $VERSION"
# -retracted, so a fix release that retracts $VERSION still lists it and
# itself. A failed lookup warns instead of reading as "none".
if versions=$(cd "$work" && go list -m -retracted -versions "$MODULE" 2>&1) && [ -n "$versions" ]; then
    newer=""
    for v in $(printf '%s\n' "$versions" | cut -d' ' -f2-); do
        [ "$v" = "$VERSION" ] && continue
        top=$(printf '%s\n%s\n' "$VERSION" "$v" | sort -V | tail -n 1)
        [ "$top" = "$v" ] && newer="$newer $v"
    done
    if [ -n "$newer" ]; then
        msg="$MODULE has released$newer since $VERSION. Test it and drop the patched copy (agent/third_party/websocket/OVERWATCH.md)."
        echo "::warning::$msg"
        echo "$msg"
    else
        echo "ok: none yet"
    fi
else
    msg="could not list $MODULE releases, so a fixed release may have gone unnoticed: $versions"
    echo "::warning::$msg"
    echo "$msg"
fi
