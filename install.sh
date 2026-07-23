#!/bin/sh
# install.sh — universal installer for Witself release binaries.
#
#   # witself (default):
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
#   # witself-infra:
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra
#   # witself-admin:
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-admin
#
# Downloads the selected binary for your OS/arch from the GitHub releases,
# verifies its SHA-256 checksum, and installs it on your PATH.
#
# Usage:
#   sh                 install latest witself
#   sh -s BINARY       install latest witself, witself-infra, witself-server, or witself-admin
#   sh -s BINARY VER   install a specific binary version
#   sh -s VER          install a specific witself version
#
# Environment:
#   WITSELF_BINARY   back-compat binary selector; positional BINARY wins
#   WS_VERSION       version to install (e.g. v0.0.1); default: latest release
#   WS_INSTALL_DIR   install directory; default: /usr/local/bin (sudo if needed)
#   WS_RELEASE_DIR   absolute directory containing an archive and checksums.txt;
#                    requires an explicit version and skips all network access
#   WS_INSTALL_LOCK_TIMEOUT
#                    seconds to wait for another installer; default: 30
#
# Note: witself-infra drives the `pulumi` engine at runtime. Unlike `brew install`
# (which pulls it automatically), this installer cannot — install pulumi yourself.

set -eu

REPO="witwave-ai/witself"
INSTALL_DIR="${WS_INSTALL_DIR:-/usr/local/bin}"
RELEASE_DIR="${WS_RELEASE_DIR:-}"

err() { printf 'install: %s\n' "$1" >&2; exit 1; }
info() { printf '%s\n' "$1" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

BINARY="${WITSELF_BINARY:-witself}"
version="${WS_VERSION:-}"

case "${1:-}" in
  "")
    [ "$#" -eq 0 ] || err "empty binary/version argument"
    ;;
  witself | ws | witself-infra | witself-server | witself-admin)
    BINARY="$1"
    version="${2:-${WS_VERSION:-}}"
    [ "$#" -le 2 ] || err "too many arguments (usage: sh -s [BINARY] [VERSION])"
    ;;
  *)
    version="$1"
    [ "$#" -le 1 ] || err "too many arguments (usage: sh -s [BINARY] [VERSION])"
    ;;
esac

# "ws" is the muscle-memory alias for the renamed tenant CLI.
[ "$BINARY" = "ws" ] && BINARY="witself"

case "$BINARY" in
  witself | witself-infra | witself-server | witself-admin) ;;
  *) err "unknown binary \"${BINARY}\" (want witself|witself-infra|witself-server|witself-admin)" ;;
esac

download() { # url dest
  if have curl; then curl -fsSL "$1" -o "$2"
  elif have wget; then wget -qO "$2" "$1"
  else err "need curl or wget"; fi
}
fetch() { # url -> stdout
  if have curl; then curl -fsSL "$1"
  elif have wget; then wget -qO- "$1"
  else err "need curl or wget"; fi
}

# Detect OS and architecture.
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (linux and darwin only)" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) err "unsupported architecture: $arch (amd64 and arm64 only)" ;;
esac

# Resolve the version: positional arg, then WS_VERSION, then the latest release.
if [ -z "$version" ]; then
  [ -z "$RELEASE_DIR" ] || err "WS_RELEASE_DIR requires an explicit version"
  info "Resolving latest ${BINARY} release..."
  version=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" |
    grep '"tag_name"' | head -1 | sed -e 's/.*"tag_name":[[:space:]]*"//' -e 's/".*//')
  [ -n "$version" ] || err "could not resolve the latest version"
fi
# The tag carries a leading v; the asset name uses the version without it.
case "$version" in
  v*) tag="$version"; ver="${version#v}" ;;
  *) tag="v$version"; ver="$version" ;;
esac
case "$ver" in
  "" | *[!A-Za-z0-9._+-]*) err "invalid version \"${version}\"" ;;
esac

asset="${BINARY}_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

if [ -n "$RELEASE_DIR" ]; then
  case "$RELEASE_DIR" in
    /*) ;;
    *) err "WS_RELEASE_DIR must be an absolute path" ;;
  esac
  [ -d "$RELEASE_DIR" ] || err "WS_RELEASE_DIR is not a directory: ${RELEASE_DIR}"
  RELEASE_DIR=$(CDPATH='' cd -P "$RELEASE_DIR" 2>/dev/null && pwd) \
    || err "could not resolve WS_RELEASE_DIR: ${RELEASE_DIR}"
fi

info "Installing ${BINARY} ${tag} (${os}/${arch})..."

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t ws-install)

dest=""
dest_use_sudo=0
lock_dir=""
lock_owner_file=""
lock_held=0
txn_dir=""
staged_binary=""
staged_alias=""
primary_path=""
alias_path=""
primary_backup=""
primary_restore=""
alias_backup=""
alias_restore=""
primary_quarantine=""
alias_quarantine=""
prior_primary_kind="none"
prior_primary_identity=""
prior_primary_hash=""
prior_primary_link=""
prior_primary_runnable=0
prior_alias_kind="none"
prior_alias_identity=""
prior_alias_link=""
staged_binary_identity=""
staged_binary_hash=""
staged_alias_identity=""
commit_started=0
primary_replaced=0
alias_replaced=0
transaction_committed=0
preserve_transaction=0

run_dest() {
  if [ "$dest_use_sudo" -eq 1 ]; then
    sudo "$@"
  else
    "$@"
  fi
}

self_test_binary() { # path
  "$1" version >/dev/null 2>&1
}

version_output_matches_request() { # output
  case "$1" in
    "${BINARY} ${ver}" | "${BINARY} ${ver} ("*")") return 0 ;;
    *) return 1 ;;
  esac
}

destination_file_hash() { # path
  if have sha256sum; then
    run_dest sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    run_dest shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

path_identity() { # path
  run_dest ls -di "$1" 2>/dev/null | awk 'NR == 1 {print $1}'
}

target_matches_snapshot() { # path kind identity hash link
  snapshot_path=$1
  snapshot_kind=$2
  snapshot_identity=$3
  snapshot_hash=$4
  snapshot_link=$5
  case "$snapshot_kind" in
    none)
      [ ! -e "$snapshot_path" ] && [ ! -L "$snapshot_path" ]
      ;;
    file)
      [ -f "$snapshot_path" ] && [ ! -L "$snapshot_path" ] \
        && [ "$(path_identity "$snapshot_path")" = "$snapshot_identity" ] \
        && [ "$(destination_file_hash "$snapshot_path")" = "$snapshot_hash" ]
      ;;
    symlink)
      [ -L "$snapshot_path" ] \
        && [ "$(path_identity "$snapshot_path")" = "$snapshot_identity" ] \
        && [ "$(readlink "$snapshot_path")" = "$snapshot_link" ]
      ;;
    *)
      return 1
      ;;
  esac
}

restore_quarantined_path() { # canonical quarantine
  restore_canonical=$1
  restore_quarantine=$2
  if [ -L "$restore_quarantine" ]; then
    restore_link=$(readlink "$restore_quarantine") || return 1
    run_dest ln -s -- "$restore_link" "$restore_canonical" \
      && run_dest rm -f "$restore_quarantine"
    return
  fi
  if [ -f "$restore_quarantine" ]; then
    run_dest ln "$restore_quarantine" "$restore_canonical" \
      && run_dest rm -f "$restore_quarantine"
    return
  fi
  return 1
}

quarantine_installed_target() { # canonical quarantine expected-kind identity hash link
  quarantine_canonical=$1
  quarantine_path=$2
  quarantine_kind=$3
  quarantine_identity=$4
  quarantine_hash=$5
  quarantine_link=$6
  [ ! -e "$quarantine_path" ] && [ ! -L "$quarantine_path" ] || return 1
  [ -e "$quarantine_canonical" ] || [ -L "$quarantine_canonical" ] || return 1
  run_dest mv "$quarantine_canonical" "$quarantine_path" || return 1
  if target_matches_snapshot \
    "$quarantine_path" "$quarantine_kind" "$quarantine_identity" "$quarantine_hash" "$quarantine_link"; then
    return 0
  fi

  # The live path changed after commit. Put those later bytes back only when
  # their canonical name is still vacant; never overwrite another concurrent
  # writer. Returning failure retains the verified previous backup.
  restore_quarantined_path "$quarantine_canonical" "$quarantine_path" || :
  return 1
}

restore_prior_target() { # canonical prior-kind backup restore link
  restore_canonical=$1
  restore_kind=$2
  restore_backup=$3
  restore_stage=$4
  restore_link=$5
  [ ! -e "$restore_canonical" ] && [ ! -L "$restore_canonical" ] || return 1
  case "$restore_kind" in
    file)
      run_dest cp -p "$restore_backup" "$restore_stage" \
        && run_dest cmp -s "$restore_backup" "$restore_stage" \
        && run_dest ln "$restore_stage" "$restore_canonical" \
        && run_dest cmp -s "$restore_backup" "$restore_canonical"
      ;;
    symlink)
      run_dest ln -s -- "$restore_link" "$restore_canonical" \
        && [ -L "$restore_canonical" ] \
        && [ "$(readlink "$restore_canonical")" = "$restore_link" ]
      ;;
    none)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

rollback_target() { # canonical quarantine installed-kind identity hash link prior-kind backup restore prior-link
  rollback_canonical=$1
  rollback_quarantine=$2
  rollback_installed_kind=$3
  rollback_installed_identity=$4
  rollback_installed_hash=$5
  rollback_installed_link=$6
  rollback_prior_kind=$7
  rollback_backup=$8
  rollback_restore=$9
  shift 9
  rollback_prior_link=$1

  quarantine_installed_target \
    "$rollback_canonical" "$rollback_quarantine" \
    "$rollback_installed_kind" "$rollback_installed_identity" \
    "$rollback_installed_hash" "$rollback_installed_link" \
    || return 1
  if ! restore_prior_target \
    "$rollback_canonical" "$rollback_prior_kind" \
    "$rollback_backup" "$rollback_restore" "$rollback_prior_link"; then
    return 1
  fi
  run_dest rm -f "$rollback_quarantine"
}

rollback_install() {
  rollback_ok=1

  if [ "$primary_replaced" -eq 1 ]; then
    rollback_target \
      "$primary_path" "$primary_quarantine" file \
      "$staged_binary_identity" "$staged_binary_hash" "" \
      "$prior_primary_kind" "$primary_backup" "$primary_restore" "$prior_primary_link" \
      || rollback_ok=0
  fi

  if [ "$alias_replaced" -eq 1 ]; then
    rollback_target \
      "$alias_path" "$alias_quarantine" symlink \
      "$staged_alias_identity" "" "witself" \
      "$prior_alias_kind" "$alias_backup" "$alias_restore" "$prior_alias_link" \
      || rollback_ok=0
  fi

  if [ "$rollback_ok" -eq 1 ] && [ "$prior_primary_runnable" -eq 1 ]; then
    self_test_binary "$primary_path" || rollback_ok=0
  fi

  if [ "$rollback_ok" -eq 1 ]; then
    if [ "$prior_primary_kind" = "none" ]; then
      info "Removed failed ${BINARY} installation."
    else
      info "Restored the previous ${BINARY} installation."
    fi
    return 0
  fi

  info "WARNING: automatic rollback of ${BINARY} did not complete."
  info "Transaction recovery artifacts remain in ${txn_dir}; inspect them before retrying."
  return 1
}

cleanup() {
  cleanup_status=$1
  trap - 0 INT TERM HUP
  set +e

  if [ "$commit_started" -eq 1 ] && [ "$transaction_committed" -eq 0 ]; then
    if ! rollback_install; then
      cleanup_status=1
      preserve_transaction=1
    fi
  fi

  if [ "$preserve_transaction" -eq 0 ]; then
    for cleanup_path in \
      "$staged_binary" "$staged_alias" \
      "$primary_backup" "$primary_restore" \
      "$alias_backup" "$alias_restore" \
      "$primary_quarantine" "$alias_quarantine"
    do
      [ -n "$cleanup_path" ] && run_dest rm -f "$cleanup_path" >/dev/null 2>&1
    done
    if [ -n "$txn_dir" ]; then
      run_dest rmdir "$txn_dir" >/dev/null 2>&1
    fi
  fi

  if [ "$lock_held" -eq 1 ]; then
    run_dest rm -f "$lock_owner_file" >/dev/null 2>&1
    run_dest rmdir "$lock_dir" >/dev/null 2>&1
  fi

  rm -rf "$tmp"
  exit "$cleanup_status"
}

trap 'cleanup $?' 0
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [ -n "$RELEASE_DIR" ]; then
  for name in "$asset" checksums.txt; do
    source_path="${RELEASE_DIR}/${name}"
    [ -f "$source_path" ] && [ ! -L "$source_path" ] \
      || err "local release asset is missing or not a regular file: ${source_path}"
    cp "$source_path" "${tmp}/${name}" \
      || err "could not copy local release asset: ${source_path}"
  done
else
  download "${base}/${asset}" "${tmp}/${asset}"
  download "${base}/checksums.txt" "${tmp}/checksums.txt"
fi

# Verify the SHA-256 checksum before trusting the binary.
expected=$(awk -v f="$asset" '$2 == f {print $1}' "${tmp}/checksums.txt")
[ -n "$expected" ] || err "no checksum found for ${asset}"
if have sha256sum; then actual=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
elif have shasum; then actual=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
else err "need sha256sum or shasum to verify the download"; fi
[ "$expected" = "$actual" ] || err "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"
info "Checksum verified."

# Extract.
archive_entries="${tmp}/archive-entries.txt"
archive_types="${tmp}/archive-types.txt"
LC_ALL=C tar -tzf "${tmp}/${asset}" >"${archive_entries}" \
  || err "could not inspect archive ${asset}"
LC_ALL=C tar -tvzf "${tmp}/${asset}" | cut -c1 >"${archive_types}" \
  || err "could not inspect archive entry types for ${asset}"
entry_count=$(wc -l <"${archive_entries}" | tr -d '[:space:]')
type_count=$(wc -l <"${archive_types}" | tr -d '[:space:]')
[ "$entry_count" = 1 ] && [ "$type_count" = 1 ] \
  && [ "$(sed -n '1p' "${archive_entries}")" = "$BINARY" ] \
  && [ "$(sed -n '1p' "${archive_types}")" = "-" ] \
  || err "archive ${asset} must contain exactly one regular root entry named ${BINARY}"
tar -xzf "${tmp}/${asset}" -C "${tmp}"
[ -f "${tmp}/${BINARY}" ] && [ ! -L "${tmp}/${BINARY}" ] \
  || err "binary ${BINARY} not found as a regular file in the archive"
chmod +x "${tmp}/${BINARY}"

# Select a destination before acquiring its installer-wide lock. The lock covers
# every Witself binary so concurrently installing different release assets cannot
# leave a partially updated command set or alias.
select_destination() {
  if mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
    dest="$INSTALL_DIR"
    dest_use_sudo=0
    return 0
  fi

  if have sudo && sudo mkdir -p "$INSTALL_DIR"; then
    info "Elevating with sudo to write $INSTALL_DIR..."
    dest="$INSTALL_DIR"
    dest_use_sudo=1
    return 0
  fi

  [ -n "${HOME:-}" ] || return 1
  fallback_dir="${HOME}/.local/bin"
  if mkdir -p "$fallback_dir" 2>/dev/null && [ -d "$fallback_dir" ] && [ -w "$fallback_dir" ]; then
    dest="$fallback_dir"
    dest_use_sudo=0
    info "Installing to ~/.local/bin — ensure it is on your PATH."
    return 0
  fi

  if have sudo && sudo mkdir -p "$fallback_dir"; then
    info "Elevating with sudo to write $fallback_dir..."
    dest="$fallback_dir"
    dest_use_sudo=1
    info "Installing to ~/.local/bin — ensure it is on your PATH."
    return 0
  fi

  return 1
}

acquire_install_lock() {
  lock_timeout="${WS_INSTALL_LOCK_TIMEOUT:-30}"
  case "$lock_timeout" in
    "" | *[!0-9]*) err "WS_INSTALL_LOCK_TIMEOUT must be a non-negative integer" ;;
  esac

  lock_dir="${dest}/.witself-install.lock"
  lock_owner_file="${lock_dir}/owner"
  lock_waited=0
  lock_announced=0
  current_uid=$(id -u 2>/dev/null || printf 'unknown')

  while :; do
    if run_dest mkdir "$lock_dir" 2>/dev/null; then
      lock_held=1
      printf '%s %s\n' "$$" "$current_uid" > "${tmp}/lock-owner"
      run_dest cp "${tmp}/lock-owner" "$lock_owner_file" \
        || err "could not record installer lock owner in ${lock_dir}"
      return 0
    fi

    # Reclaim only a same-user lock whose recorded process is definitely gone.
    # A missing or malformed owner file may be an installer between mkdir and cp,
    # so it is never treated as stale.
    lock_owner=$(run_dest cat "$lock_owner_file" 2>/dev/null || :)
    lock_pid=$(printf '%s\n' "$lock_owner" | awk 'NR == 1 {print $1}')
    lock_uid=$(printf '%s\n' "$lock_owner" | awk 'NR == 1 {print $2}')
    case "$lock_pid" in
      "" | *[!0-9]*) lock_pid="" ;;
    esac
    case "$lock_uid" in
      "" | *[!0-9]*) lock_uid="" ;;
    esac
    if [ -n "$lock_pid" ] && [ "$lock_uid" = "$current_uid" ] \
      && ! kill -0 "$lock_pid" 2>/dev/null; then
      info "Removing stale installer lock in ${dest}..."
      run_dest rm -f "$lock_owner_file" >/dev/null 2>&1 || :
      run_dest rmdir "$lock_dir" >/dev/null 2>&1 || :
      continue
    fi

    if [ "$lock_waited" -ge "$lock_timeout" ]; then
      err "another Witself installation is in progress for ${dest} (lock: ${lock_dir})"
    fi
    if [ "$lock_announced" -eq 0 ]; then
      info "Waiting for another Witself installation in ${dest}..."
      lock_announced=1
    fi
    sleep 1
    lock_waited=$((lock_waited + 1))
  done
}

select_destination || err "could not install to ${INSTALL_DIR} or ~/.local/bin"
dest=$(CDPATH='' cd -P "$dest" 2>/dev/null && pwd) \
  || err "could not resolve install directory: ${dest}"
acquire_install_lock

# Stage the executable in the destination filesystem and run it before changing
# any live path. The final mv operations are same-filesystem atomic renames.
txn_dir=$(run_dest mktemp -d "${dest}/.witself-install.XXXXXX") \
  || err "could not create an install transaction in ${dest}"
run_dest chmod 0755 "$txn_dir" \
  || err "could not prepare install transaction in ${dest}"
staged_binary="${txn_dir}/${BINARY}.candidate"
run_dest cp "${tmp}/${BINARY}" "$staged_binary" \
  || err "could not stage ${BINARY} in ${dest}"
run_dest chmod 0755 "$staged_binary" \
  || err "could not make staged ${BINARY} executable"
staged_version=$("$staged_binary" version 2>&1) \
  || err "staged ${BINARY} failed to run; the existing installation was not changed"
version_output_matches_request "$staged_version" \
  || err "staged ${BINARY} reported a version other than requested ${tag}; the existing installation was not changed"
staged_binary_identity=$(path_identity "$staged_binary") \
  || err "could not record staged ${BINARY} file identity"
staged_binary_hash=$(destination_file_hash "$staged_binary") \
  || err "could not hash staged ${BINARY}"
info "Staged ${BINARY} passed its runtime and version checks."

primary_path="${dest}/${BINARY}"
primary_backup="${txn_dir}/${BINARY}.previous"
primary_restore="${txn_dir}/${BINARY}.restore"
primary_quarantine="${txn_dir}/${BINARY}.rollback-target"
if [ -L "$primary_path" ]; then
  prior_primary_kind="symlink"
  prior_primary_identity=$(path_identity "$primary_path") \
    || err "could not record existing ${primary_path} identity"
  prior_primary_link=$(readlink "$primary_path") \
    || err "could not inspect existing ${primary_path}"
  run_dest ln -s -- "$prior_primary_link" "$primary_backup" \
    || err "could not preserve existing symlink ${primary_path}"
  target_matches_snapshot \
    "$primary_path" "$prior_primary_kind" "$prior_primary_identity" "" "$prior_primary_link" \
    || err "existing ${primary_path} changed while it was backed up"
  self_test_binary "$primary_path" && prior_primary_runnable=1 || :
elif [ -e "$primary_path" ]; then
  [ -f "$primary_path" ] \
    || err "existing install target is not a regular file: ${primary_path}"
  prior_primary_kind="file"
  prior_primary_identity=$(path_identity "$primary_path") \
    || err "could not record existing ${primary_path} identity"
  prior_primary_hash=$(destination_file_hash "$primary_path") \
    || err "could not hash existing ${primary_path}"
  if ! run_dest cp -p "$primary_path" "$primary_backup" \
    || ! run_dest cmp -s "$primary_path" "$primary_backup"; then
    err "could not make a verified backup of ${primary_path}"
  fi
  target_matches_snapshot \
    "$primary_path" "$prior_primary_kind" "$prior_primary_identity" "$prior_primary_hash" "" \
    || err "existing ${primary_path} changed while it was backed up"
  self_test_binary "$primary_path" && prior_primary_runnable=1 || :
fi

# Prepare and preserve the tenant alias before the first live rename. Only the
# exact ws -> witself symlink created by this installer is considered owned;
# every other pre-existing path is left untouched and requires user resolution.
if [ "$BINARY" = "witself" ]; then
  alias_path="${dest}/ws"
  alias_backup="${txn_dir}/ws.previous"
  alias_restore="${txn_dir}/ws.restore"
  alias_quarantine="${txn_dir}/ws.rollback-target"
  if [ -L "$alias_path" ]; then
    prior_alias_kind="symlink"
    prior_alias_identity=$(path_identity "$alias_path") \
      || err "could not record existing ${alias_path} identity"
    prior_alias_link=$(readlink "$alias_path") \
      || err "could not inspect existing ${alias_path}"
    [ "$prior_alias_link" = "witself" ] \
      || err "refusing to replace non-Witself alias at ${alias_path} (target: ${prior_alias_link})"
    run_dest ln -s -- "$prior_alias_link" "$alias_backup" \
      || err "could not preserve existing symlink ${alias_path}"
    target_matches_snapshot \
      "$alias_path" "$prior_alias_kind" "$prior_alias_identity" "" "$prior_alias_link" \
      || err "existing ${alias_path} changed while it was backed up"
  elif [ -e "$alias_path" ]; then
    err "refusing to replace non-Witself path at ${alias_path}"
  fi

  staged_alias="${txn_dir}/ws.candidate"
  run_dest ln -s -- "witself" "$staged_alias" \
    || err "could not stage the ws alias"
  staged_alias_identity=$(path_identity "$staged_alias") \
    || err "could not record staged ws alias identity"
fi

# Commit the primary and alias, then execute the committed paths while the
# verified backups and lock are still held. Any failure exits through cleanup,
# which restores the exact prior files (or removes a failed first install).
target_matches_snapshot \
  "$primary_path" "$prior_primary_kind" "$prior_primary_identity" "$prior_primary_hash" "$prior_primary_link" \
  || err "existing ${primary_path} changed before commit; the installation was not changed"
if [ "$BINARY" = "witself" ]; then
  target_matches_snapshot \
    "$alias_path" "$prior_alias_kind" "$prior_alias_identity" "" "$prior_alias_link" \
    || err "existing ${alias_path} changed before commit; the installation was not changed"
fi
commit_started=1
run_dest mv -f "$staged_binary" "$primary_path" \
  || err "could not atomically install ${BINARY} to ${primary_path}"
primary_replaced=1
target_matches_snapshot \
  "$primary_path" file "$staged_binary_identity" "$staged_binary_hash" "" \
  || err "installed ${BINARY} changed during commit; refusing unsafe rollback"
if [ "$BINARY" = "witself" ]; then
  run_dest mv -f "$staged_alias" "$alias_path" \
    || err "could not atomically install the ws alias"
  alias_replaced=1
  target_matches_snapshot \
    "$alias_path" symlink "$staged_alias_identity" "" "witself" \
    || err "installed ws alias changed during commit; refusing unsafe rollback"
fi

installed_version=$("$primary_path" version 2>&1) \
  || err "installed ${BINARY} failed to run after commit; restoring the previous installation"
version_output_matches_request "$installed_version" \
  || err "installed ${BINARY} reported a version other than requested ${tag}; restoring the previous installation"

if [ "$BINARY" = "witself" ]; then
  [ -L "$alias_path" ] && [ "$(readlink "$alias_path")" = "witself" ] \
    || err "installed ws alias is inconsistent; restoring the previous installation"
  alias_version=$("$alias_path" version 2>&1) \
    || err "installed ws alias failed to run; restoring the previous installation"
  version_output_matches_request "$alias_version" \
    || err "installed ws alias reported a version other than requested ${tag}; restoring the previous installation"
  [ "$alias_version" = "$installed_version" ] \
    || err "installed ws alias reported a different version from ${BINARY}; restoring the previous installation"
fi

transaction_committed=1
info "Installed ${BINARY} to ${primary_path}"
if [ "$BINARY" = "witself" ]; then
  info "Aliased ${alias_path} -> witself"
fi
printf '%s\n' "$installed_version"

if [ "$BINARY" = "witself-infra" ] && ! have pulumi; then
  info ""
  info "Note: witself-infra drives the 'pulumi' engine at runtime, which this"
  info "installer does not fetch. Install it with:  brew install pulumi"
  info "  (or: curl -fsSL https://get.pulumi.com | sh)"
fi
