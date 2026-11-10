#!/bin/sh
# Review-only. Creates the unprivileged claude-worker user and its directories.
# Does nothing unless called with --apply, by root, after review.
set -eu

if [ "${1:-}" != "--apply" ]; then
	echo "review-only: rerun as root with --apply to create the claude-worker user" >&2
	exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
	echo "must run as root to create a system user" >&2
	exit 1
fi

if ! id claude-worker >/dev/null 2>&1; then
	useradd --system --home-dir /nonexistent --no-create-home \
		--shell /usr/sbin/nologin --user-group claude-worker
fi

# Membership in docker (or sudo/adm) would hand the worker root on this host.
for group in docker sudo adm wheel; do
	if id -nG claude-worker | tr ' ' '\n' | grep -qx "$group"; then
		echo "claude-worker is in group $group; remove it before continuing" >&2
		exit 1
	fi
done

install -d -o root -g claude-worker -m 0750 /etc/claude-worker
install -d -o root -g root -m 0755 /opt/claude-worker/bin /opt/claude-code/bin

if [ ! -f /etc/claude-worker/bearer-token ]; then
	umask 077
	head -c 48 /dev/urandom | base64 | tr -d '\n=+/' > /etc/claude-worker/bearer-token
	chown claude-worker:claude-worker /etc/claude-worker/bearer-token
	chmod 0600 /etc/claude-worker/bearer-token
fi

echo "claude-worker user and directories are ready; the OAuth token is the owner's step"
