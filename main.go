// mynas is a small private NAS server: a web file manager plus external links,
// offline downloads and WebDAV mounts. It is one binary with the web UI embedded.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// App is everything that works on one files directory. A Host builds it and
// replaces it when the directory changes (see host.go).
type App struct {
	host    *Host
	st      *Store
	auth    *Auth
	files   *Files
	links   *Links
	dls     *Downloads
	mounts  *Mounts
	dav     *Dav
	handler http.Handler

	inflight sync.WaitGroup // requests being served by this App
}

// Options configures a Host.
type Options struct {
	DataDir   string // holds state.json
	FilesDir  string // holds the user's files; defaults to DataDir/files
	FilesHook string // command run with a directory newly chosen in the web UI; see Host.hook
	BindHost  string // host the link proxies listen on
	PortMin   int
	PortMax   int
	MinFree   uint64 // bytes that must stay free on the files volume
}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// NewApp starts a Host and returns the App it serves with.
func NewApp(o Options) (*App, error) {
	h, err := NewHost(o)
	if err != nil {
		return nil, err
	}
	return h.cur.Load(), nil
}

// newApp builds the parts for one files directory, which must exist. It does
// not start any background work; start does.
func newApp(h *Host, dir string) (*App, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	o := h.opts
	a := &App{host: h, st: h.st, auth: h.auth}
	a.files = &Files{st: h.st, root: root, dir: dir, minFree: o.MinFree, trashSeen: map[string]time.Time{}, stop: make(chan struct{})}
	a.links = NewLinks(h.st, h.auth, o.BindHost, o.PortMin, o.PortMax)
	a.dls = NewDownloads(h.st, a.files, 3)
	a.mounts = &Mounts{st: h.st, dl: a.dls}
	a.dav = NewDav(h.st, a.files)
	a.handler = a.routes()
	return a, nil
}

// start launches the link proxies, the unfinished downloads and the recycle bin clean-up.
func (a *App) start() {
	a.links.Start()
	a.dls.Start()
	go a.files.trashLoop()
}

// close stops what start launched. The directory handle stays open so that
// requests still running against this App can finish.
func (a *App) close() {
	a.links.StopAll()
	close(a.files.stop)
}

// Handler serves the web UI and the API. It follows changes of the files directory.
func (a *App) Handler() http.Handler { return a.host }

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	auth := a.auth.require

	mux.HandleFunc("GET /api/session", a.auth.handleSession)
	mux.HandleFunc("POST /api/setup", a.auth.handleSetup)
	mux.HandleFunc("POST /api/login", a.auth.handleLogin)
	mux.HandleFunc("POST /api/logout", a.auth.handleLogout)
	mux.HandleFunc("PUT /api/account", auth(a.auth.handleAccount))

	mux.HandleFunc("GET /api/files/list", auth(a.files.handleList))
	mux.HandleFunc("GET /api/files/find", auth(a.files.handleFind))
	mux.HandleFunc("GET /api/files/download", auth(a.files.handleDownload))
	mux.HandleFunc("POST /api/files/upload", auth(a.files.handleUpload))
	mux.HandleFunc("POST /api/files/mkdir", auth(a.files.handleMkdir))
	mux.HandleFunc("POST /api/files/rename", auth(a.files.handleRename))
	mux.HandleFunc("POST /api/files/delete", auth(a.files.handleDelete))
	mux.HandleFunc("GET /api/trash", auth(a.files.handleTrashList))
	mux.HandleFunc("POST /api/trash/restore", auth(a.files.handleTrashRestore))
	mux.HandleFunc("POST /api/trash/purge", auth(a.files.handleTrashPurge))
	mux.HandleFunc("GET /api/storage", auth(a.files.handleStorage))
	mux.HandleFunc("GET /api/settings", auth(a.host.handleSettings))
	mux.HandleFunc("PUT /api/settings", auth(a.files.handleSettingsPut))
	mux.HandleFunc("PUT /api/settings/files-dir", auth(a.host.handleSetFilesDir))
	mux.HandleFunc("GET /api/hostdirs", auth(a.host.handleHostDirs))

	mux.HandleFunc("GET /api/dav", auth(a.dav.handleList))
	mux.HandleFunc("POST /api/dav", auth(a.dav.handleCreate))
	mux.HandleFunc("DELETE /api/dav/{id}", auth(a.dav.handleDelete))
	mux.HandleFunc("POST /api/dav/{id}/to-space", auth(a.dav.handleToSpace))
	mux.HandleFunc("POST /api/dav/{id}/init", auth(a.dav.handleInit))
	mux.HandleFunc("POST /api/dav/{id}/password", auth(a.dav.handleNewPassword))
	mux.HandleFunc("GET /api/spaces", auth(a.dav.handleSpaces))
	mux.HandleFunc("DELETE /api/spaces/{name}", auth(a.dav.handleSpaceDelete))
	// The WebDAV endpoint has its own login (access keys), not the web session.
	mux.Handle("/dav", a.dav)
	mux.Handle("/dav/", a.dav)

	mux.HandleFunc("GET /api/links", auth(a.links.handleList))
	mux.HandleFunc("POST /api/links", auth(a.links.handleCreate))
	mux.HandleFunc("PUT /api/links/{id}", auth(a.links.handleUpdate))
	mux.HandleFunc("DELETE /api/links/{id}", auth(a.links.handleDelete))
	mux.HandleFunc("GET /api/links/{id}/check", auth(a.links.handleCheck))

	mux.HandleFunc("GET /api/mounts", auth(a.mounts.handleList))
	mux.HandleFunc("POST /api/mounts", auth(a.mounts.handleCreate))
	mux.HandleFunc("POST /api/mounts/test", auth(a.mounts.handleTest))
	mux.HandleFunc("PUT /api/mounts/{id}", auth(a.mounts.handleUpdate))
	mux.HandleFunc("DELETE /api/mounts/{id}", auth(a.mounts.handleDelete))
	mux.HandleFunc("GET /api/mounts/{id}/list", auth(a.mounts.handleBrowse))
	mux.HandleFunc("GET /api/mounts/{id}/download", auth(a.mounts.handleDownload))
	mux.HandleFunc("POST /api/mounts/{id}/save", auth(a.mounts.handleSave))

	mux.HandleFunc("GET /api/downloads", auth(a.dls.handleList))
	mux.HandleFunc("POST /api/downloads", auth(a.dls.handleCreate))
	mux.HandleFunc("POST /api/downloads/{id}/{action}", auth(a.dls.handleAction))
	mux.HandleFunc("DELETE /api/downloads/{id}", auth(a.dls.handleDelete))

	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/favicon.svg", http.StatusFound)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "接口不存在")
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServerFS(sub))
	return secure(mux)
}

const uiCSP = "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; " +
	"style-src 'self' 'unsafe-inline'; frame-src http: https:; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'self'"

// secure adds the security headers and rejects cross-site writes: every
// state-changing API call must carry a header that only same-origin scripts can set.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "mynas" {
				writeErr(w, http.StatusForbidden, "请求被拒绝")
				return
			}
		} else {
			h.Set("Content-Security-Policy", uiCSP)
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

func parsePortRange(s string) (int, int, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("端口范围格式应为 起始-结束，例如 18600-18649")
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(lo))
	b, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
		return 0, 0, fmt.Errorf("端口范围不正确: %s", s)
	}
	return a, b, nil
}

func main() {
	addr := flag.String("addr", envOr("MYNAS_ADDR", ":8580"), "管理页面的监听地址")
	dataDir := flag.String("data", envOr("MYNAS_DATA", "data"), "数据目录（存放配置 state.json）")
	filesDir := flag.String("files", envOr("MYNAS_FILES", ""), "文件目录（默认是数据目录下的 files）")
	minFree := flag.Int("min-free", envInt("MYNAS_MIN_FREE", 64), "文件所在磁盘至少保留的空闲空间（MB），低于它时拒绝上传和下载")
	filesHook := flag.String("files-hook", envOr("MYNAS_FILES_HOOK", ""), "在管理界面更改存储位置时执行的命令（参数是新目录），用于把设置写回系统配置")
	showVersion := flag.Bool("version", false, "显示版本号")
	ports := flag.String("proxy-ports", envOr("MYNAS_PROXY_PORTS", "18600-18649"), "外部链接代理内嵌使用的端口范围")
	flag.Parse()
	if *showVersion {
		fmt.Println("mynas " + version)
		return
	}
	if *minFree < 0 {
		log.Fatal("min-free 不能为负数")
	}

	portMin, portMax, err := parsePortRange(*ports)
	if err != nil {
		log.Fatal(err)
	}
	bind, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("监听地址不正确: %v", err)
	}
	host, err := NewHost(Options{
		DataDir: *dataDir, FilesDir: *filesDir, FilesHook: *filesHook,
		BindHost: bind, PortMin: portMin, PortMax: portMax, MinFree: uint64(*minFree) << 20,
	})
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: *addr, Handler: host, ReadHeaderTimeout: 15 * time.Second}
	log.Printf("mynas %s 已启动：管理页面 http://%s ，文件目录 %s", version, displayAddr(*addr), host.cur.Load().files.dir)
	log.Fatal(srv.ListenAndServe())
}

func displayAddr(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
