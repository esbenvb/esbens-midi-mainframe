#!/usr/bin/env bash
# Verify that Buildroot accepted every line of our defconfig.
#
#   check-defconfig.sh DEFCONFIG DOTCONFIG [MAKE_LOG]
#
# Kconfig silently drops symbols whose dependencies are not met, and only warns
# about symbols that do not exist. Since nobody can test the image without a long
# build, this runs right after "make <board>_defconfig" (seconds) and fails the
# build if anything we asked for did not make it into .config.
set -euo pipefail

defconfig=${1:?usage: check-defconfig.sh DEFCONFIG DOTCONFIG [MAKE_LOG]}
dotconfig=${2:?}
log=${3:-}
fail=0

if [ -n "$log" ] && [ -f "$log" ]; then
  # Unknown symbols (typos, or a symbol that moved between Buildroot releases) are fatal.
  if grep -Eq 'nonexistent symbol|trying to assign' "$log"; then
    echo "check-defconfig: Kconfig does not know some symbols in $defconfig:" >&2
    grep -E 'nonexistent symbol|trying to assign' "$log" >&2
    fail=1
  fi
  # Other warnings are shown but not fatal; the per-line check below decides.
  if grep -E '^[^ ]*: *warning|^warning' "$log" | grep -Evq 'nonexistent symbol|trying to assign'; then
    echo "check-defconfig: note: Kconfig printed warnings (see $log):" >&2
    grep -E '^[^ ]*: *warning|^warning' "$log" | grep -Ev 'nonexistent symbol|trying to assign' | head -10 >&2
  fi
fi

while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    '' ) continue ;;
    '# BR2_'*' is not set')
      sym=${line#\# }; sym=${sym%% *}
      if grep -qE "^$sym=" "$dotconfig"; then
        echo "check-defconfig: $sym should be off but is: $(grep -E "^$sym=" "$dotconfig") (another option selects it)" >&2
        fail=1
      fi ;;
    '#'* ) continue ;;
    BR2_*=* )
      if ! grep -qxF -- "$line" "$dotconfig"; then
        sym=${line%%=*}
        got=$(grep -E "^$sym=|^# $sym is not set" "$dotconfig" || true)
        echo "check-defconfig: wanted   $line" >&2
        echo "check-defconfig: .config has ${got:-<nothing: unknown symbol or unmet dependency>}" >&2
        fail=1
      fi ;;
  esac
done < "$defconfig"

if [ "$fail" != 0 ]; then
  echo "check-defconfig: FAILED: fix image/defconfig.d/ (then image/mkconfigs.sh)" >&2
  exit 1
fi
echo "check-defconfig: all $(grep -cE '^(BR2_|# BR2_)' "$defconfig") settings accepted"
