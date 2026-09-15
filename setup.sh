#!/bin/sh
# Aurium setup — clone to running dashboard, one command.
#
# Deliberately POSIX sh with no make, no git and no python: on macOS all three
# are disabled until the Xcode licence is accepted, which is one of the exact
# conditions this script exists to survive. It never uses sudo and never writes
# outside $HOME, so piping it from curl is a defensible thing to ask of anyone.
set -eu

GO_VERSION=go1.27.1
GO_FLOOR_MINOR=25
PREFIX="${HOME}/.local"
# shellcheck disable=SC1007 # intentional: clears CDPATH only for this cd, so
# a CDPATH set in the caller's environment can't make cd print a match or
# resolve somewhere unexpected.
REPO_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

detect_platform() {
	case $(uname -s) in
		Darwin) OS=darwin ;;
		Linux)  OS=linux ;;
		*) die "unsupported OS $(uname -s); this script covers macOS and Linux" ;;
	esac
	case $(uname -m) in
		arm64|aarch64) ARCH=arm64 ;;
		x86_64|amd64)  ARCH=amd64 ;;
		*) die "unsupported architecture $(uname -m)" ;;
	esac
}

# Judge go by running it. A binary that exists and fails is the failure mode
# this whole script is built around.
go_is_usable() {
	command -v go >/dev/null 2>&1 || return 1
	v=$(go version 2>/dev/null) || return 1
	minor=$(printf '%s' "$v" | sed -n 's/.*go1\.\([0-9][0-9]*\).*/\1/p')
	[ -n "$minor" ] && [ "$minor" -ge "$GO_FLOOR_MINOR" ]
}

install_go() {
	tarball="${GO_VERSION}.${OS}-${ARCH}.tar.gz"
	want=$(awk -v f="$tarball" '$1==f {print $2}' "${REPO_DIR}/go-checksums.txt")
	[ -n "$want" ] || die "no checksum recorded for ${tarball}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	say "installing ${GO_VERSION} for ${OS}/${ARCH} into ${PREFIX}/go"
	curl -fL --retry 1 -o "${tmp}/go.tar.gz" "https://go.dev/dl/${tarball}" ||
		die "download failed; fetch https://go.dev/dl/${tarball} by hand and extract it to ${PREFIX}/go"

	if command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "${tmp}/go.tar.gz" | awk '{print $1}')
	else
		got=$(sha256sum "${tmp}/go.tar.gz" | awk '{print $1}')
	fi
	# Never retried: a mismatch means the bytes are not the bytes we pinned.
	[ "$got" = "$want" ] || die "checksum mismatch for ${tarball}: got ${got}, want ${want}"

	mkdir -p "$PREFIX"
	rm -rf "${PREFIX}/go"
	tar -C "$PREFIX" -xzf "${tmp}/go.tar.gz"
}

detect_platform

fresh_install=0
if go_is_usable; then
	GO=go
else
	if [ ! -x "${PREFIX}/go/bin/go" ]; then
		install_go
		fresh_install=1
	fi
	GO="${PREFIX}/go/bin/go"
	# The build below calls $GO by absolute path, so it doesn't strictly need
	# this — but doctor's own "go" check, which the handoff at the end of
	# this script runs, looks go up on PATH by name. Without this export a
	# bootstrap that installs and builds everything correctly would still end
	# by reporting go as missing.
	export PATH="${PREFIX}/go/bin:${PATH}"
fi
say "using $("$GO" version)"

# Only on a fresh install: someone who already has ~/.local/go/bin/go from a
# prior run has presumably already seen this, or is fine without it (a CI
# container, say). Printed, never written to a shell profile — rewriting a
# stranger's .zshrc unasked is exactly the kind of thing that makes a
# `curl | sh` script untrustworthy.
if [ "$fresh_install" -eq 1 ]; then
	say "note: ${PREFIX}/go/bin is on PATH for this run only; add it to your shell profile to keep it there:"
	say "  export PATH=\"\$HOME/.local/go/bin:\$PATH\""
fi

# CGO off because Aurium's SQLite (modernc.org/sqlite) is pure Go — so an
# unusable clang cannot stop the build. -buildvcs=false because git may be
# present and refuse to run, and VCS stamping reports that as "exit status 69".
export CGO_ENABLED=0
BUILD="$GO build -buildvcs=false"

cd "$REPO_DIR"
say "building"
for c in aurium auriumd aurium-mcp; do
	$BUILD -o "bin/${c}" "./cmd/${c}"
done
for a in amd64 arm64; do
	GOOS=linux GOARCH="$a" $BUILD -o "bin/linux-${a}/aurium-mcp" ./cmd/aurium-mcp
done
mkdir -p "${HOME}/.aurium/bin"
cp bin/linux-amd64/aurium-mcp "${HOME}/.aurium/bin/aurium-mcp-linux-amd64"
cp bin/linux-arm64/aurium-mcp "${HOME}/.aurium/bin/aurium-mcp-linux-arm64"

say ""
exec ./bin/aurium doctor --fix
