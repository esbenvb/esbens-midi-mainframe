# shellcheck shell=bash
# Shared helpers for the EMM scripts. Source this file; do not execute it.

if [ -t 2 ]; then
  _c_red=$'\033[31m'; _c_yel=$'\033[33m'; _c_grn=$'\033[32m'; _c_off=$'\033[0m'
else
  _c_red=''; _c_yel=''; _c_grn=''; _c_off=''
fi

log()  { printf '%s==>%s %s\n' "$_c_grn" "$_c_off" "$*" >&2; }
warn() { printf '%sWARNING:%s %s\n' "$_c_yel" "$_c_off" "$*" >&2; }
die()  { printf '%sERROR:%s %s\n' "$_c_red" "$_c_off" "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# DRY_RUN=1 makes run() print instead of execute. Read-only checks never go
# through run(); anything that changes the system or a remote machine does.
DRY_RUN=${DRY_RUN:-0}
run() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '+' >&2
    printf ' %q' "$@" >&2
    printf '\n' >&2
  else
    "$@"
  fi
}

# run_quiet CMD...: run() for a command whose own stderr should be hidden. The
# redirect must not sit on the call itself ("run cmd 2>/dev/null"): in a dry run
# it would swallow the plan line that run() prints on stderr.
run_quiet() {
  if [ "$DRY_RUN" = 1 ]; then
    run "$@"
  else
    "$@" 2>/dev/null
  fi
}

# print_usage_comment FILE: print the comment block that follows the #! line of
# FILE (up to the first line that is not a comment), without the leading "# ".
# Used for --help, so the help text is exactly the header comment and cannot
# drift into code lines when the header grows or shrinks.
print_usage_comment() {
  awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$1"
}

# write_file PATH MODE  (content on stdin). Honours DRY_RUN.
write_file() {
  local path=$1 mode=$2
  if [ "$DRY_RUN" = 1 ]; then
    printf '+ write %s (mode %s)\n' "$path" "$mode" >&2
    cat >/dev/null
  else
    install -d "$(dirname "$path")"
    cat > "$path"
    chmod "$mode" "$path"
  fi
}

# Version string baked into the binary (git describe, falling back to "dev").
emm_version() {
  local root=${1:-.}
  git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev
}

# Cross-compile the daemon for 64-bit Raspberry Pi OS / the appliance image.
# Usage: build_emmd <repo-root> [version]
build_emmd() {
  local root=$1 version=${2:-}
  have go || die "Go is required to build emmd (https://go.dev/dl/). Or pass a prebuilt binary."
  [ -n "$version" ] || version=$(emm_version "$root")
  log "building emmd $version for linux/arm64"
  mkdir -p "$root/dist"
  (
    cd "$root"
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
      go build -trimpath -ldflags "-s -w -X main.version=$version" \
      -o dist/emmd-linux-arm64 ./cmd/emmd
  )
  printf '%s\n' "$version" > "$root/dist/VERSION"
}
