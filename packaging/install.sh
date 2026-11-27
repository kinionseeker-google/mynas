#!/bin/sh
# Installs the mynas package that matches this router.
# Copy this script and the package files into one directory on the router
# (for example /tmp/mynas) and run:  sh install.sh

dir="$(cd "$(dirname "$0")" && pwd)"

[ -f /etc/openwrt_release ] || { echo "这不是 OpenWrt 系统" >&2; exit 1; }
. /etc/openwrt_release

case "$DISTRIB_ARCH" in
	aarch64*) cpu=aarch64 ;;
	x86_64*) cpu=x86_64 ;;
	mipsel*) cpu=mipsel ;;
	mips_* | mips) cpu=mips ;;
	arm_* | arm) cpu=arm ;;
	*) echo "暂不支持这种处理器：${DISTRIB_ARCH:-未知}" >&2; exit 1 ;;
esac

if command -v apk >/dev/null 2>&1; then
	pkg="$(ls "$dir"/mynas-*_"$cpu".apk 2>/dev/null | tail -n 1)"
	[ -n "$pkg" ] || { echo "目录里没有 $cpu 的 .apk 安装包" >&2; exit 1; }
	echo "系统 $DISTRIB_RELEASE（$DISTRIB_ARCH），安装 $(basename "$pkg")"
	apk add --allow-untrusted "$pkg" || exit 1
else
	pkg="$(ls "$dir"/mynas_*_"$cpu".ipk 2>/dev/null | tail -n 1)"
	[ -n "$pkg" ] || { echo "目录里没有 $cpu 的 .ipk 安装包" >&2; exit 1; }
	echo "系统 $DISTRIB_RELEASE（$DISTRIB_ARCH），安装 $(basename "$pkg")"
	opkg install "$pkg" || exit 1
fi

ip="$(uci -q get network.lan.ipaddr)"
ip="${ip%%/*}"
echo
echo "安装完成。"
echo "管理界面：http://${ip:-路由器地址}:$(uci -q get mynas.main.port)"
echo "文件目录：$(uci -q get mynas.main.files_dir)"
echo "修改设置：LuCI 的“服务 → 黑曜石云盘”（英文界面是 Services）"
