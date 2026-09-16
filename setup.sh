#!/bin/sh
# Aurium setup — clone to running dashboard, one command.
#
# Run it from a checkout: `./setup.sh`. It is not a curl-pipe installer and
# does not try to be one — it reads go-checksums.txt from its own directory and
# builds the tree it sits in, so it needs the clone to already exist. Piped
# from curl there is no clone and no checksum file, and it would stop.
#
# What it does promise is that you can read the whole thing before running it:
# no sudo, no writes outside $HOME, and the one download it makes is verified
# against a SHA256 committed to this repository.
#
# Deliberately POSIX sh with no make, no git and no python: on macOS all three
# are disabled until the Xcode licence is accepted, which is one of the exact
# conditions this script exists to survive.
#
#   ./setup.sh             daemon and dashboard
#   ./setup.sh --desktop   also the native window, which needs a Rust toolchain
#
# --desktop is opt-in because Rust plus the Tauri CLI is roughly 1.5 GB and
# several minutes of compiling, and most people running this want the daemon.
# If you only want the app, do not build it — download the .dmg from Releases.
set -eu

WANT_DESKTOP=0
for arg in "$@"; do
	case "$arg" in
	--desktop) WANT_DESKTOP=1 ;;
	-h|--help)
		say() { printf '%s\n' "$*"; }
		say "usage: ./setup.sh [--desktop]"
		say "  --desktop  also build the native window (installs Rust; ~1.5 GB)"
		exit 0
		;;
	*) printf 'error: unknown option %s\n' "$arg" >&2; exit 2 ;;
	esac
done

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
#
# Takes the candidate to test — a bare name to look up on PATH, or an absolute
# path — because a toolchain at ${PREFIX}/go/bin from some earlier run deserves
# exactly the same floor test as one on PATH. Adopted unexamined, a stale one
# there means a GO_VERSION bump silently never installs and the build dies on
# `go.mod requires go >= 1.25` instead of getting the clean reinstall this
# script exists to perform.
go_is_usable() {
	candidate=$1
	command -v "$candidate" >/dev/null 2>&1 || return 1
	v=$("$candidate" version 2>/dev/null) || return 1
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
	# --proto/--proto-redir pin the transfer to https, redirects included. The
	# committed checksum below already makes a downgrade unexploitable; this is
	# free defence in depth for the one thing this script fetches.
	curl -fL --proto '=https' --proto-redir '=https' --retry 1 \
		-o "${tmp}/go.tar.gz" "https://go.dev/dl/${tarball}" ||
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

	# Not left to the EXIT trap above: this script's last line is `exec
	# ./bin/aurium doctor --fix`, and exec replaces the shell's process image
	# instead of exiting it, so an EXIT trap set here never fires on the
	# success path. The trap still covers every early-exit and die() path
	# before this point; this is only for the one it can't reach.
	rm -rf "$tmp"
}

# build_desktop installs the Rust toolchain if absent and builds the native
# window. Kept behind --desktop because it is by far the heaviest thing here:
# roughly 1.5 GB and several minutes, against a daemon build measured in
# seconds. rustup is invoked with --no-modify-path for the same reason the Go
# install is: rewriting a stranger's shell profile unasked is not this script's
# business.
build_desktop() {
	if [ "$OS" != "darwin" ] && [ "$OS" != "linux" ]; then
		die "--desktop supports macOS and Linux only"
	fi
	if ! command -v cargo >/dev/null 2>&1 && [ ! -x "${HOME}/.cargo/bin/cargo" ]; then
		say "installing the Rust toolchain (~1.5 GB, several minutes)"
		curl --proto '=https' --proto-redir '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
			-o "${TMPDIR:-/tmp}/rustup-init.sh" || die "could not download rustup"
		sh "${TMPDIR:-/tmp}/rustup-init.sh" -y --no-modify-path --profile minimal \
			|| die "rustup install failed"
		rm -f "${TMPDIR:-/tmp}/rustup-init.sh"
	fi
	export PATH="${HOME}/.cargo/bin:${PATH}"

	if ! command -v cargo-tauri >/dev/null 2>&1; then
		say "installing the Tauri CLI (compiles from source, a few minutes)"
		cargo install tauri-cli --version '^2' --locked || die "tauri-cli install failed"
	fi

	say "building the native window"
	# The bundle step is best-effort: create-dmg drives Finder through
	# AppleScript to lay out the disk image window, which fails without
	# automation permission. The .app is the thing that matters, and a
	# released .dmg is built in CI where that problem does not arise.
	( cd "${REPO_DIR}/desktop" && cargo tauri build ) || \
		say "  note: bundling did not complete; the built app is under desktop/src-tauri/target/release/"

	# Verify the signature rather than assume it. tauri.conf.json sets an
	# ad-hoc signingIdentity, but a bundle that silently skipped signing is
	# indistinguishable from a signed one until macOS refuses to open it --
	# and then it says "Aurium is damaged", which sends people hunting for a
	# corrupt download instead of a missing signature. v0.1.0 shipped that
	# way. The check costs milliseconds; the alternative cost a release.
	app="${REPO_DIR}/desktop/src-tauri/target/release/bundle/macos/Aurium.app"
	if [ "$OS" = darwin ] && [ -d "$app" ]; then
		if codesign --verify --deep --strict "$app" >/dev/null 2>&1; then
			say "  signature verified (ad-hoc; macOS will still ask you to allow it once)"
		else
			say "  warning: the built app is not correctly signed. macOS will call it"
			say "  damaged and refuse to open it. Sign it before copying it anywhere:"
			say "    codesign --force --deep --sign - \"$app\""
		fi
	fi
}

detect_platform

fresh_install=0
if go_is_usable go; then
	GO=go
else
	# Same floor test, not a bare existence check: install_go starts by
	# removing ${PREFIX}/go, so reinstalling over a stale toolchain is the
	# clean path rather than a special case.
	if ! go_is_usable "${PREFIX}/go/bin/go"; then
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

# tmux is a host-side convenience: `aurium attach` uses it to drop you into a
# running agent. Installing it needs no privileges when Homebrew is present,
# so it is offered rather than merely reported. Homebrew itself is not
# installed here — that needs sudo, and this script does not escalate.
if ! command -v tmux >/dev/null 2>&1; then
	if command -v brew >/dev/null 2>&1; then
		say "installing tmux (host-side convenience for \`aurium attach\`)"
		brew_log="${TMPDIR:-/tmp}/aurium-brew-tmux.log"
		if brew install tmux >"$brew_log" 2>&1; then
			say "  tmux installed"
		else
			# Show brew's own reason rather than telling someone to re-run the
			# command that just failed. A Homebrew whose lock directory is not
			# writable needs a chown this script will not perform, and "run brew
			# install tmux" would fail identically — the kind of advice this
			# project exists to stop giving.
			say "  tmux install failed. Homebrew said:"
			if ! grep -iE '^(Error|Warning):' "$brew_log" | head -3 | sed 's/^/    /'; then
				tail -3 "$brew_log" | sed 's/^/    /'
			fi
			say "  Carrying on without it: tmux is optional, only aurium attach uses it."
			say "  Full output: $brew_log"
		fi
	else
		say "note: tmux is not installed and Homebrew is not here to install it."
		say "  tmux is optional — only \`aurium attach\` uses it."
		say "  To get both, install Homebrew first (it will ask for your password):"
		say "    /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\""
		say "  then: brew install tmux"
	fi
fi

if [ "$WANT_DESKTOP" -eq 1 ]; then
	build_desktop
fi

say ""
exec ./bin/aurium doctor --fix
