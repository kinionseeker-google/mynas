#!/bin/sh
# Starts mynas. With require_mount it first waits until the files directory is
# on a mounted disk, so nothing is written to the router's flash while the disk
# is still missing (early during boot, or after the disk was unplugged).
#
#   run.sh <require_mount 0|1> <files_dir> [mynas flags...]
#   run.sh check <files_dir>        exit 0 when files_dir is on a mounted disk

on_disk() {
	local p="$1" mp
	while [ ! -d "$p" ]; do
		p="${p%/*}"
		[ -n "$p" ] || p=/
	done
	mp="$(df -P "$p" 2>/dev/null | awk 'NR==2 {print $NF}')"
	case "$mp" in
		"" | / | /overlay | /rom | /tmp) return 1 ;;
	esac
	return 0
}

if [ "$1" = check ]; then
	on_disk "$2"
	exit $?
fi

require_mount="$1"
files_dir="$2"
shift 2
state=/var/run/mynas.state

if [ "$require_mount" = 1 ] && ! on_disk "$files_dir"; then
	echo waiting > "$state"
	logger -t mynas "waiting for the disk that holds $files_dir"
	while ! on_disk "$files_dir"; do
		sleep 5
	done
	logger -t mynas "disk mounted, starting"
fi

mkdir -p "$files_dir" || exit 1
echo running > "$state"
exec /usr/bin/mynas -files "$files_dir" "$@"
