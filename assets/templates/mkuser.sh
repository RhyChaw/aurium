#!/bin/sh
# Create a user with a specific uid/gid, working across alpine (busybox
# adduser), debian/ubuntu (useradd) and images where the uid already exists.
#
# Matching the host uid is what makes identical-path bind mounts usable: the
# agent writes to the real repository, and the human still owns the files.
set -e

UID_WANT="$1"
GID_WANT="$2"
NAME="$3"

# If something already owns this uid, reuse it rather than failing the build.
EXISTING="$(getent passwd "$UID_WANT" 2>/dev/null | cut -d: -f1 || true)"
if [ -n "$EXISTING" ]; then
	if [ "$EXISTING" != "$NAME" ]; then
		echo "mkuser: uid $UID_WANT already belongs to '$EXISTING'; reusing it as the agent user" >&2
		NAME="$EXISTING"
	fi
	mkdir -p /home/aurium
	chown "$UID_WANT:$GID_WANT" /home/aurium 2>/dev/null || true
	exit 0
fi

if ! getent group "$GID_WANT" >/dev/null 2>&1; then
	if command -v groupadd >/dev/null 2>&1; then
		groupadd -g "$GID_WANT" "$NAME"
	else
		addgroup -g "$GID_WANT" "$NAME"
	fi
fi
GROUP_NAME="$(getent group "$GID_WANT" | cut -d: -f1)"

if command -v useradd >/dev/null 2>&1; then
	useradd -u "$UID_WANT" -g "$GID_WANT" -m -d /home/aurium -s /bin/bash "$NAME"
else
	adduser -u "$UID_WANT" -G "$GROUP_NAME" -h /home/aurium -s /bin/sh -D "$NAME"
fi

mkdir -p /home/aurium
chown -R "$UID_WANT:$GID_WANT" /home/aurium
