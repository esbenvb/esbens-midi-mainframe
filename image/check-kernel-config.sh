#!/usr/bin/env bash
# Verify the generated kernel .config against the requirement files (board/emm/kernel-required.txt
# and the board's own kernel-required.<board>.txt).
#   check-kernel-config.sh KERNEL_DOTCONFIG REQUIRED_FILE [REQUIRED_FILE...]
set -euo pipefail

cfg=${1:?usage: check-kernel-config.sh KERNEL_DOTCONFIG REQUIRED_FILE [REQUIRED_FILE...]}
shift
[ "$#" -gt 0 ] || { echo "check-kernel-config: no requirement file given" >&2; exit 2; }
fail=0

for req in "$@"; do
  # A missing file must fail the check, not silently drop its requirements.
  [ -f "$req" ] || { echo "check-kernel-config: requirement file not found: $req" >&2; exit 2; }
  while read -r sym allowed _ || [ -n "${sym:-}" ]; do
    case "${sym:-}" in ''|'#'*) continue ;; esac
    got=$(grep -E "^CONFIG_${sym}=" "$cfg" | head -n1 | cut -d= -f2 || true)
    got=${got:-n}
    ok=0
    IFS=, read -r -a alts <<< "$allowed"
    for a in "${alts[@]}"; do [ "$got" = "$a" ] && ok=1; done
    if [ "$ok" = 0 ]; then
      echo "check-kernel-config: CONFIG_$sym is '$got', need one of: $allowed (from $(basename "$req"))" >&2
      fail=1
    fi
  done < "$req"
done

if [ "$fail" != 0 ]; then
  echo "check-kernel-config: FAILED (adjust board/emm/linux.fragment)" >&2
  exit 1
fi
echo "check-kernel-config: kernel options OK"
