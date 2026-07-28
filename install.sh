#!/usr/bin/env sh
# pm — one-line installer for macOS and Linux.
#
# Downloads the latest pm release from GitHub, verifies its SHA-256 digest
# against the release's SHA256SUMS, and installs the binary. The script only
# writes the `pm` binary; it never starts a daemon or modifies configuration.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sudo sh
#   curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sh -s -- --install-dir ~/.local/bin
#   curl -fsSL https://raw.githubusercontent.com/CoolBanHub/pm/main/install.sh | sh -s -- --version v0.0.7
#
# Environment:
#   PM_INSTALL_DIR   override the install directory (same as --install-dir)

set -eu

REPO="CoolBanHub/pm"
SYSTEM_DIR="/usr/local/bin"
USER_DIR="${HOME}/.local/bin"

PM_INSTALL_DIR="${PM_INSTALL_DIR:-}"
PM_VERSION="${PM_VERSION:-}"
USE_SUDO=0
tmpdir=""

if [ -t 1 ]; then
    BLUE='\033[34m'; YELLOW='\033[33m'; RED='\033[31m'; RESET='\033[0m'
else
    BLUE=''; YELLOW=''; RED=''; RESET=''
fi

info() { printf '%b==>%b %s\n' "$BLUE" "$RESET" "$*"; }
warn() { printf '%b==>%b %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die()  { printf '%b==>%b %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

usage() {
    cat <<'EOF'
Usage: sh install.sh [options]

Options:
  --install-dir DIR   install into DIR (default: /usr/local/bin, or
                      ~/.local/bin when /usr/local/bin is not writable)
  --version TAG       install a specific release tag (default: latest)
  -h, --help          show this help

Environment:
  PM_INSTALL_DIR      same as --install-dir
EOF
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --install-dir) shift; PM_INSTALL_DIR="${1:?--install-dir requires a value}";;
            --install-dir=*) PM_INSTALL_DIR="${1#--install-dir=}";;
            --version) shift; PM_VERSION="${1:?--version requires a value}";;
            --version=*) PM_VERSION="${1#--version=}";;
            -h|--help) usage; exit 0;;
            *) die "unknown option: $1 (use --install-dir or --version)";;
        esac
        shift
    done
}

detect_platform() {
    case "$(uname -s)" in
        Darwin) PM_OS=darwin ;;
        Linux)  PM_OS=linux ;;
        *)      die "unsupported OS '$(uname -s)'; pm ships darwin/linux binaries";;
    esac
    case "$(uname -m)" in
        x86_64|amd64)  PM_ARCH=amd64 ;;
        aarch64|arm64) PM_ARCH=arm64 ;;
        i386|i686)     PM_ARCH=386 ;;
        *)             die "unsupported architecture '$(uname -m)'";;
    esac
}

# Resolve the latest release tag. Tries the GitHub API first, then falls back
# to following the /releases/latest redirect (keeps working under rate limits).
latest_tag() {
    tag=$(curl --connect-timeout 30 --max-time 60 -fsSL \
          "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
          | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' \
          | head -n 1) || true
    if [ -n "$tag" ]; then printf '%s\n' "$tag"; return 0; fi
    url=$(curl --connect-timeout 30 --max-time 60 -fsSLI -o /dev/null -w '%{url_effective}' \
          "https://github.com/${REPO}/releases/latest" 2>/dev/null) || true
    tag=$(printf '%s\n' "$url" | sed -n 's|.*/tag/\(.*\)$|\1|p')
    if [ -n "$tag" ]; then printf '%s\n' "$tag"; return 0; fi
    return 1
}

download() {
    url=$1; dest=$2
    info "Downloading ${url}"
    attempts=3
    i=0
    while [ "$i" -lt "$attempts" ]; do
        i=$((i + 1))
        if curl --connect-timeout 30 --max-time 300 --http1.1 -fsSL "$url" -o "$dest"; then
            return 0
        fi
        rm -f "$dest"
        warn "download attempt $i/$attempts failed; retrying..."
        sleep 2
    done
    die "download failed after $attempts attempts: $url"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else die "neither sha256sum nor shasum is available"; fi
}

verify_checksum() {
    file=$1; sums=$2
    name=$(basename "$file")
    expected=$(grep -E "[[:space:]]${name}\$" "$sums" | awk '{print $1}' | head -n 1)
    [ -n "$expected" ] || die "no checksum entry for ${name} in SHA256SUMS"
    actual=$(sha256_file "$file")
    [ "$actual" = "$expected" ] || \
        die "checksum mismatch for ${name} (expected ${expected}, got ${actual})"
    info "Verified SHA-256: ${actual}"
}

# Decide where to install. /usr/local/bin if writable (or root); otherwise sudo
# when a password can be prompted, else fall back to the user's bin directory.
resolve_bindir() {
    if [ -n "$PM_INSTALL_DIR" ]; then
        printf '%s' "$PM_INSTALL_DIR"; return
    fi
    if [ -w "$SYSTEM_DIR" ] || [ "$(id -u)" -eq 0 ]; then
        printf '%s' "$SYSTEM_DIR"; return
    fi
    if command -v sudo >/dev/null 2>&1 && [ -t 0 ]; then
        USE_SUDO=1; printf '%s' "$SYSTEM_DIR"; return
    fi
    warn "$SYSTEM_DIR is not writable; falling back to $USER_DIR."
    warn "Add it to your PATH, or rerun with: curl -fsSL .../install.sh | sudo sh"
    printf '%s' "$USER_DIR"
}

cleanup() { [ -n "$tmpdir" ] && rm -rf "$tmpdir" || true; }
trap cleanup EXIT

main() {
    parse_args "$@"
    need_cmd curl; need_cmd tar

    detect_platform
    [ -n "$PM_VERSION" ] || PM_VERSION="$(latest_tag)" \
        || die "could not determine the latest release; pass --version <tag>"

    tmpdir="$(mktemp -d 2>/dev/null)" || die "could not create a temporary directory"

    asset="pm-${PM_OS}-${PM_ARCH}.tar.gz"
    base="https://github.com/${REPO}/releases/download/${PM_VERSION}"
    info "Installing pm ${PM_VERSION} for ${PM_OS}/${PM_ARCH}"

    download "${base}/${asset}"   "${tmpdir}/${asset}"
    download "${base}/SHA256SUMS" "${tmpdir}/SHA256SUMS"
    verify_checksum "${tmpdir}/${asset}" "${tmpdir}/SHA256SUMS"

    tar -C "$tmpdir" -xzf "${tmpdir}/${asset}"

    bindir="$(resolve_bindir)"
    sudocmd=""
    [ "$USE_SUDO" -eq 1 ] && sudocmd=sudo
    $sudocmd mkdir -p "$bindir" 2>/dev/null || true
    info "Installing to ${bindir}/pm"
    $sudocmd cp "$tmpdir/pm" "${bindir}/pm" \
        || die "failed to write ${bindir}/pm (try: curl ... | sudo sh)"
    $sudocmd chmod 0755 "${bindir}/pm"

    info "Installed:"
    "${bindir}/pm" -v 2>/dev/null || warn "could not run ${bindir}/pm -v"

    existing="$(command -v pm 2>/dev/null || true)"
    if [ -n "$existing" ] && [ "$existing" != "${bindir}/pm" ]; then
        warn "Another 'pm' is already on PATH at ${existing}; the new one is at ${bindir}/pm."
        warn "Adjust your PATH or remove the old binary."
    elif [ "$bindir" = "$USER_DIR" ]; then
        warn "Ensure ${USER_DIR} is on your PATH, e.g.: export PATH=\"${USER_DIR}:\$PATH\""
    else
        info "Done. Run 'pm help' to get started; update later with 'pm update'."
    fi
}

main "$@"
