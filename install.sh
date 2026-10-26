#!/bin/sh
# Installs gv, the Grepvine CLI, from its GitHub releases.
#
#   curl -fsSL https://grepvine-ai.github.io/grepvine-cli/install.sh | sh
#   curl -fsSL https://grepvine-ai.github.io/grepvine-cli/install.sh | sh -s -- --version v0.1.1 --bin-dir ~/bin
#
# GV_VERSION and GV_INSTALL_DIR can be set instead of the flags.
#
# Everything runs from main, called on the last line, so a partly downloaded
# script runs nothing.
set -eu

REPO=grepvine-ai/grepvine-cli
RELEASES="https://github.com/$REPO/releases"

usage() {
	cat <<EOF
Install gv, the Grepvine CLI, from $RELEASES.

Usage: install.sh [--version vX.Y.Z] [--bin-dir DIR]

  -v, --version   Release to install (default: the latest release, or \$GV_VERSION)
  -b, --bin-dir   Directory to install gv into (default: \$GV_INSTALL_DIR or ~/.local/bin)
  -h, --help      Show this help
EOF
}

say() {
	printf 'gv-install: %s\n' "$*" >&2
}

fail() {
	say "$*"
	exit 1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# fetch URL [DEST] writes URL to DEST, or to stdout without DEST.
fetch() {
	if have curl; then
		if [ $# -gt 1 ]; then
			curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"
		else
			curl -fsSL --proto '=https' --tlsv1.2 -H 'Accept: application/json' "$1"
		fi
	else
		if [ $# -gt 1 ]; then
			wget -q -O "$2" "$1"
		else
			wget -q --header='Accept: application/json' -O - "$1"
		fi
	fi
}

latest_version() {
	# The release page answers with JSON when asked for it, which avoids the
	# rate limit on unauthenticated GitHub API requests.
	fetch "$RELEASES/latest" | sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p'
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		fail "this script supports Linux and macOS. On Windows, download the zip archive from $RELEASES/latest"
		;;
	*) fail "no gv build for $(uname -s). Build it with: go install github.com/$REPO/cmd/gv@latest" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) fail "no gv build for $(uname -m). Build it with: go install github.com/$REPO/cmd/gv@latest" ;;
	esac
}

sha256() {
	if have sha256sum; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif have shasum; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	else
		fail "sha256sum or shasum is needed to verify the download. Install one and run this script again."
	fi
}

main() {
	version=${GV_VERSION:-}
	bin_dir=${GV_INSTALL_DIR:-${HOME:?set HOME or pass --bin-dir}/.local/bin}

	while [ $# -gt 0 ]; do
		case "$1" in
		-v | --version)
			[ $# -ge 2 ] || fail "$1 needs a release, for example $1 v0.1.1"
			version=$2
			shift 2
			;;
		-b | --bin-dir)
			[ $# -ge 2 ] || fail "$1 needs a directory, for example $1 /usr/local/bin"
			bin_dir=$2
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			fail "unknown argument $1"
			;;
		esac
	done

	have curl || have wget || fail "curl or wget is needed to download gv. Install one and run this script again."
	have tar || fail "tar is needed to unpack gv. Install it and run this script again."

	os=$(detect_os)
	arch=$(detect_arch)

	if [ -z "$version" ]; then
		version=$(latest_version) || true
		[ -n "$version" ] || fail "could not find the latest release at $RELEASES/latest. Check your connection and retry, or pass --version."
	fi
	case "$version" in
	v*) ;;
	*) version="v$version" ;;
	esac

	archive="gv_${version#v}_${os}_${arch}.tar.gz"
	base="$RELEASES/download/$version"

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t gv-install)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	say "downloading gv $version for $os/$arch"
	fetch "$base/$archive" "$tmp/$archive" ||
		fail "could not download $base/$archive. Check that $version is listed at $RELEASES, check your connection, and retry."
	fetch "$base/checksums.txt" "$tmp/checksums.txt" ||
		fail "could not download $base/checksums.txt. Check your connection and retry."

	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "checksums.txt for $version does not list $archive. Retry, or download gv from $RELEASES."
	got=$(sha256 "$tmp/$archive")
	[ "$want" = "$got" ] || fail "the checksum of $archive does not match checksums.txt. Retry, and if it fails again, report it as described in SECURITY.md."

	tar -xzf "$tmp/$archive" -C "$tmp" gv ||
		fail "could not unpack $archive. Retry, or download gv from $RELEASES."

	mkdir -p "$bin_dir" 2>/dev/null ||
		fail "could not create $bin_dir. Pass a directory you can write to with --bin-dir."
	[ -w "$bin_dir" ] ||
		fail "$bin_dir is not writable. Pass a directory you can write to with --bin-dir, or run the script with sudo."
	# Copy next to the target and rename, so a running gv is never left half
	# written.
	cp "$tmp/gv" "$bin_dir/.gv.$$"
	chmod 755 "$bin_dir/.gv.$$"
	mv -f "$bin_dir/.gv.$$" "$bin_dir/gv"

	"$bin_dir/gv" --version >/dev/null 2>&1 ||
		fail "$bin_dir/gv was installed but does not run on this system. Build it instead with: go install github.com/$REPO/cmd/gv@latest"
	say "installed gv $version to $bin_dir/gv"
	case ":$PATH:" in
	*":$bin_dir:"*) ;;
	*) say "$bin_dir is not on your PATH. Add it, for example: export PATH=\"$bin_dir:\$PATH\"" ;;
	esac
	say "run 'gv login' to sign in"
}

main "$@"
