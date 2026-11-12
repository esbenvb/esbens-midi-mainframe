#!/usr/bin/env bash
# Test the emmd launcher (board/emm/overlay/usr/libexec/emm-launch) against fake emmd files.
# No Pi needed: the launcher's paths are redirected into a temporary folder.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
L=$HERE/board/emm/overlay/usr/libexec/emm-launch
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0

# fake NAME VERSION_EXIT OSAPI: an "emmd" that prints NAME when started with no flag.
fake() { # path name version_exit os_api
  cat > "$1" <<EOS
#!/bin/sh
case "\$1" in
  --version) exit $3 ;;
  --os-api)  [ -n "$4" ] && echo "$4"; [ -n "$4" ] || exit 2; exit 0 ;;
  *) echo "started $2 args=\$*"; exit 0 ;;
esac
EOS
}
run() { # label expected-substring ; env: image api in $api
  local out
  out=$(EMM_BOOT=$T/boot EMM_RUN=$T/run EMM_OS_API_FILE=$T/api sh "$L" --x=1 2>&1) && rc=0 || rc=$?
  case "$out" in
    *"$2"*) echo "ok   $1" ;;
    *) echo "FAIL $1: wanted '$2', got rc=$rc: $out"; fail=1 ;;
  esac
}
reset() { rm -rf "$T/boot" "$T/run"; mkdir -p "$T/boot"; echo "${1:-1}" > "$T/api"; }

reset 1; fake "$T/boot/emmd" new 0 1;  run "current emmd starts, with its arguments" "started new args=--x=1"
reset 1; fake "$T/boot/emmd" new 1 1; fake "$T/boot/emmd.previous" old 0 1; run "broken current falls back to previous" "started old"
reset 1; fake "$T/boot/emmd" new 0 2; fake "$T/boot/emmd.previous" old 0 1; run "current needing a newer OS falls back" "started old"
reset 1; fake "$T/boot/emmd" new 0 2; run "needing a newer OS and no fallback fails" "needs system interface 2"
reset 2; fake "$T/boot/emmd" new 0 2; run "matching OS number is accepted" "started new"
reset 1; fake "$T/boot/emmd" new 0 "";  run "an emmd without --os-api counts as 0" "started new"
reset 1; printf 'not a program' > "$T/boot/emmd"; chmod 644 "$T/boot/emmd"; run "garbage is not run" "no usable emmd"
reset 1; run "empty boot partition" "no usable emmd"
rm -f "$T/api"; fake "$T/boot/emmd" new 0 1; run "missing /etc/emm-os-api counts as 0" "needs system interface 1"
exit "$fail"
