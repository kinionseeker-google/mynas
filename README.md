# 黑曜石云盘（mynas）

私有 NAS 的网盘式 Web 管理服务。界面上的名字是“黑曜石云盘”，程序、安装包和配置的名字仍是 `mynas`。一个可执行文件，界面内嵌。除 WebDAV 服务端使用 `golang.org/x/net/webdav` 外，只依赖 Go 标准库。

## 运行

```sh
go build -o mynas .
./mynas -addr :8580 -data ./data
```

打开 `http://<NAS 地址>:8580`，首次访问时创建管理员账号。

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-addr` | `MYNAS_ADDR` | `:8580` | 管理页面监听地址 |
| `-data` | `MYNAS_DATA` | `data` | 数据目录，存配置 `state.json` |
| `-files` | `MYNAS_FILES` | `<数据目录>/files` | 文件目录，可以单独放到大容量磁盘上 |
| `-min-free` | `MYNAS_MIN_FREE` | `64` | 文件所在磁盘至少保留的空闲空间（MB），低于它时拒绝上传和下载 |
| `-proxy-ports` | `MYNAS_PROXY_PORTS` | `18600-18649` | 外部链接代理内嵌使用的端口范围 |
| `-files-hook` | `MYNAS_FILES_HOOK` | | 在管理界面更改存储位置时执行的命令，参数是新目录。OpenWrt 包用它把选择写回 UCI；不设时选择保存在 `state.json` 里，并优先于 `-files` |
| `-version` | | | 显示版本号 |

Docker：`docker compose up -d --build`。

## OpenWrt

### 安装

`dist/` 里每种处理器有两个安装包：`.ipk` 用于 OpenWrt 24.10 及更早版本（opkg），`.apk` 用于 25.12 及更新版本（apk）。

最省事的办法是把 `dist/` 整个目录拷到路由器上运行 `install.sh`，它会按处理器和系统版本自动选包：

```sh
scp -O -r dist root@192.168.1.1:/tmp/mynas
ssh root@192.168.1.1 sh /tmp/mynas/install.sh
```

也可以只拷一个包手动安装。先在路由器上查处理器：`. /etc/openwrt_release; echo $DISTRIB_ARCH`

| `DISTRIB_ARCH` 开头 | 安装包 | 常见设备 |
|---|---|---|
| `aarch64` | `aarch64` | 多数较新的 ARM 路由器、软路由、树莓派 3/4/5 |
| `x86_64` | `x86_64` | x86 软路由、虚拟机 |
| `mipsel` | `mipsel` | MT7621、MT7620 等 |
| `mips_` | `mips` | 高通 ath79 等 |
| `arm_` | `arm` | 32 位 ARM（IPQ40xx、mvebu 等） |

```sh
opkg install /tmp/mynas_0.1.8-1_aarch64.ipk                    # OpenWrt 24.10 及更早
apk add --allow-untrusted /tmp/mynas-0.1.8-r1_aarch64.apk      # OpenWrt 25.12 及更新
```

装错处理器的包不会启动服务，并会提示换包。安装包没有签名，所以 apk 需要 `--allow-untrusted`。

### 安装之后

- 服务开机自启，管理界面在 `http://<路由器地址>:8580`，首次打开时创建管理员账号。
- LuCI 的“服务 → 黑曜石云盘”里可以改端口、文件目录等设置，并有打开管理界面的按钮。
- 安装时如果已经挂载了外接硬盘（`/mnt/` 下），文件目录会自动设为硬盘的挂载点（例如 `/mnt/sda1`），硬盘上已有的文件直接可见；同时开启“等磁盘挂载后再启动”，避免硬盘没挂上时把文件写进闪存。没有硬盘时文件目录是 `/mnt/mynas`（在闪存上），剩余空间不足 64 MB 时会拒绝上传和下载。
- 账号、链接、任务等配置保存在 `/etc/mynas`，系统升级（sysupgrade）时保留。
- 程序约占 8 MB 存储空间。
- 防火墙默认只允许 LAN 访问，不需要额外配置。

卸载：`opkg remove mynas` 或 `apk del mynas`。`/etc/mynas` 和文件目录会保留。

### 重新打包

```sh
go run ./packaging/mkpkg -version 0.1.8
```

`.ipk` 由这个工具直接生成；`.apk` 需要 Docker（用来运行 `apk mkpkg`），加 `-apk=off` 可以跳过。安装包内容在 `packaging/root/`，安装脚本在 `packaging/scripts/`。

## 已实现

- **文件**：浏览、上传（含拖拽）、下载、新建文件夹、重命名、搜索、按类型和最近查看、点击图片/视频/音频全屏预览（可左右切换）、回收站（还原、彻底删除、到期自动清除，默认删除满 30 天，可改或关闭）。
- **扩展 · 外部链接**：添加、编辑、固定到侧栏、检测是否可访问和可内嵌；页面内打开或新窗口打开；对禁止内嵌的网站可开启 NAS 代理内嵌。
- **设置 · 管理员账号**：在管理界面里修改管理员的用户名和密码；需要输入当前密码；修改后其他会话全部退出，当前会话保留。
- **设置 · 存储位置**：在管理界面里更改文件目录，带文件夹浏览；需要再次输入管理员密码；在线切换，不重启、不掉登录；旧位置的文件、回收站和同步空间原地保留，换回去后重新出现。
- **离线下载**：http/https 直链，暂停、继续（断点续传）、取消、重试；服务重启后自动继续未完成的任务。
- **外部网盘**：WebDAV（坚果云、Nextcloud、AList 等），只读：浏览、下载、把文件或整个文件夹保存到 NAS。
- **WebDAV 同步**：自带 WebDAV 服务端（`/dav/`），Obsidian（Remotely Save 插件）等软件可以把它当云端存储。用独立的访问密码登录，每个密码限定一个位置，可设只读，可单独移除，也可以重新生成密码（旧密码立即失效）；经它删除的文件进回收站。同步来的数据默认放在“同步空间”（文件目录下的 `.mynas-sync/<名称>`），不出现在“全部文件”、分类和搜索里，在 WebDAV 页面单独浏览；文件接口用 `?space=<名称>` 进入某个空间。

## 尚未实现

- 多用户与权限、分享链接、局域网设备发现与共享空间、SMB、磁盘健康信息。
- OpenWrt 软件源和安装包签名（目前是本地安装未签名的包）。
- 离线下载的磁力链 / BT；外部网盘的上传与删除；WebDAV 以外的网盘协议。
- 文件夹打包下载、文件索引（搜索和分类目前每次遍历目录）。

## 安全说明

- 服务本身是 HTTP。只在可信的局域网内使用；需要外网访问时放在 WireGuard / Tailscale 或 HTTPS 反向代理之后。
- 首次打开页面的人创建管理员账号，部署后请立即完成这一步。
- 管理员密码以 PBKDF2-SHA256 加盐保存。外部网盘的密码需要原样使用，因此明文保存在 `data/state.json` 中，请保护好数据目录。
- WebDAV 访问密码是 160 位随机值，只保存它的 SHA-256；它和管理员账号互不通用。
- 代理内嵌的端口只接受已登录 NAS 的浏览器，NAS 的登录凭据不会转发给被代理的网站。被代理的网站与 NAS 共用主机名，只代理你信任的服务。

## 开发

```sh
go vet ./... && go test ./...
```

## 许可证

本项目以 [MIT 许可证](LICENSE) 发布。

程序里包含 Go 标准库和 `golang.org/x/net` 的代码，它们的许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
