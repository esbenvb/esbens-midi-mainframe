#!/usr/bin/env bash
# Verify the generated kernel .config against board/emm/kernel-required.txt.
#   check-kernel-config.sh KERNEL_DOTCONFIG REQUIRED_FILE
set -euo pipefail

cfg=${1:?usage: check-kernel-config.sh KERNEL_DOTCONFIG REQUIRED_FILE}
req=${2:?}
fail=0

while read -r sym allowed _; do
  case "${sym:-}" in ''|'#'*) continue ;; esac
  got=$(grep -E "^CONFIG_${sym}=" "$cfg" | head -n1 | cut -d= -f2 || true)
  got=${got:-n}
  ok=0
  IFS=, read -r -a alts <<< "$allowed"
  for a in "${alts[@]}"; do [ "$got" = "$a" ] && ok=1; done
  if [ "$ok" = 0 ]; then
    echo "check-kernel-config: CONFIG_$sym is '$got', need one of: $allowed" >&2
    fail=1
  fi
done < "$req"

if [ "$fail" != 0 ]; then
  echo "check-kernel-config: FAILED (adjust board/emm/linux.fragment)" >&2
  exit 1
fi
echo "check-kernel-config: kernel options OK"
