#!/bin/sh
# Called by mynas when the storage location is changed in the web UI, with the
# new directory as $1. It writes the choice to UCI, so that LuCI shows it and
# the next start uses it. mynas has already switched by itself; nothing is
# restarted here.

[ -n "$1" ] || exit 1

uci -q set mynas.main.files_dir="$1" || exit 1
# Wait for the disk at boot only when the directory really is on a mounted disk.
if /bin/sh /usr/share/mynas/run.sh check "$1"; then
	uci -q set mynas.main.require_mount=1
else
	uci -q set mynas.main.require_mount=0
fi
uci commit mynas
