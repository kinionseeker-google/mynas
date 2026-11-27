package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Host owns what outlives a change of the files directory: the saved state,
// the login sessions and the HTTP entry point. Everything that works on files
// lives in an App; changing the directory builds a new App and swaps it in,
// while requests still running finish against the old one.
type Host struct {
	opts Options
	st   *Store
	auth *Auth

	// hook, when set, stores a newly chosen directory outside state.json. The
	// OpenWrt package uses it to write the choice back to UCI, so that LuCI and
	// the next start agree with the web UI.
	hook func(dir string) error

	mu   sync.Mutex // serialises directory changes
	warn string     // why the saved directory could not be used at start, if so
	cur  atomic.Pointer[App]

	// swap keeps a request from entering an App that is being replaced, so the
	// old App knows when its last request is done and can release its
	// directory. Until then the old folder could not be deleted or renamed
	// on Windows.
	swap     sync.RWMutex
	retiring sync.WaitGroup
}

// userErr is an error whose text is meant for the person using the web UI.
type userErr struct {
	code int
	msg  string
}

func (e *userErr) Error() string { return e.msg }

func uerr(code int, format string, a ...any) error {
	return &userErr{code: code, msg: fmt.Sprintf(format, a...)}
}

func NewHost(o Options) (*Host, error) {
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := OpenStore(filepath.Join(o.DataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	h := &Host{opts: o, st: st, auth: NewAuth(st)}
	if o.FilesHook != "" {
		h.hook = func(dir string) error { return runHook(o.FilesHook, dir) }
	}

	dir := o.FilesDir
	if dir == "" {
		dir = filepath.Join(o.DataDir, "files")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}

	var app *App
	// A directory chosen in the web UI wins over the command line. With a hook
	// the choice is not kept in state.json, so there is nothing to look up.
	st.mu.Lock()
	saved := ""
	if st.Settings != nil && h.hook == nil {
		saved = st.Settings.FilesDir
	}
	st.mu.Unlock()
	if saved != "" && !sameDir(saved, dir) {
		if app, err = newApp(h, saved); err != nil {
			h.warn = fmt.Sprintf("上次设置的存储位置 %s 现在无法使用，已临时改用 %s。请检查磁盘是否接好，或重新选择存储位置。", saved, dir)
			log.Printf("saved files directory %s is unusable (%v), using %s", saved, err, dir)
			app = nil
		}
	}
	if app == nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if app, err = newApp(h, dir); err != nil {
			return nil, err
		}
	}

	// Recycle bin records written before each record named its directory
	// belong to the directory in use now.
	st.mu.Lock()
	changed := false
	for _, t := range st.Trash {
		if t.Home == "" {
			t.Home, changed = app.files.dir, true
		}
	}
	if changed {
		_ = st.save()
	}
	st.mu.Unlock()

	app.start()
	h.cur.Store(app)
	return h, nil
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.swap.RLock()
	app := h.cur.Load()
	app.inflight.Add(1)
	h.swap.RUnlock()
	defer app.inflight.Done()
	app.handler.ServeHTTP(w, r)
}

// Close stops the background work of the App in use and releases the directories.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if app := h.cur.Load(); app != nil {
		app.close()
		_ = app.files.root.Close()
	}
	h.retiring.Wait()
}

func runHook(hook, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, hook, dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether child is parent itself or lies somewhere below it.
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// systemDirs lists places that must never become the files directory: the web
// UI can read and delete everything below it.
func systemDirs() (exact, trees []string) {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("SystemDrive"); d != "" {
			exact = append(exact, d+`\`)
		}
		for _, k := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
			if d := os.Getenv(k); d != "" {
				trees = append(trees, d)
			}
		}
		return exact, trees
	}
	return []string{"/"}, []string{"/proc", "/sys", "/dev", "/etc", "/bin", "/sbin", "/lib", "/usr", "/boot", "/rom", "/overlay"}
}

// checkFilesDir validates a directory chosen in the web UI and creates it when
// only its last element is missing. It returns the cleaned path.
func checkFilesDir(dir, dataDir, current string) (string, error) {
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		return "", uerr(http.StatusBadRequest, "请填写完整路径，例如 %s", exampleDir())
	}
	dir = filepath.Clean(dir)
	if sameDir(dir, current) {
		return "", uerr(http.StatusBadRequest, "这已经是当前的存储位置")
	}
	exact, trees := systemDirs()
	for _, d := range exact {
		if sameDir(dir, d) {
			return "", uerr(http.StatusBadRequest, "不能把系统盘的根目录用作存储位置，请选它下面的一个文件夹")
		}
	}
	for _, d := range trees {
		if within(dir, d) {
			return "", uerr(http.StatusBadRequest, "%s 是系统目录，不能用作存储位置", d)
		}
	}
	if abs, err := filepath.Abs(dataDir); err == nil && within(abs, dir) {
		return "", uerr(http.StatusBadRequest, "这个位置包含了黑曜石云盘自己的配置目录（%s），账号信息会暴露在文件列表里，请换一个位置", abs)
	}

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if p, perr := os.Stat(filepath.Dir(dir)); perr != nil || !p.IsDir() {
			return "", uerr(http.StatusBadRequest, "上一级文件夹 %s 不存在", filepath.Dir(dir))
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return "", uerr(http.StatusBadRequest, "无法创建这个文件夹: %v", err)
		}
	case err != nil:
		return "", uerr(http.StatusBadRequest, "无法访问这个位置: %v", err)
	case !info.IsDir():
		return "", uerr(http.StatusBadRequest, "这不是一个文件夹")
	}
	probe, err := os.CreateTemp(dir, ".mynas-check-*")
	if err != nil {
		return "", uerr(http.StatusBadRequest, "这个位置不可写，不能用作存储位置")
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return dir, nil
}

func exampleDir() string {
	if runtime.GOOS == "windows" {
		return `D:\NAS`
	}
	return "/mnt/sda1"
}

// SetFilesDir switches the files directory. Files, the recycle bin and the
// sync spaces of the old directory stay where they are and come back when the
// directory is chosen again.
func (h *Host) SetFilesDir(dir string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.cur.Load()
	clean, err := checkFilesDir(dir, h.opts.DataDir, old.files.dir)
	if err != nil {
		return err
	}
	if old.dls.active() > 0 {
		return uerr(http.StatusConflict, "有离线下载任务正在进行或排队，请等它们完成或先暂停，再更改存储位置")
	}
	app, err := newApp(h, clean)
	if err != nil {
		return uerr(http.StatusBadRequest, "无法打开这个位置: %v", err)
	}
	if h.hook != nil {
		err = h.hook(clean)
	} else {
		h.st.mu.Lock()
		if h.st.Settings == nil {
			h.st.Settings = &Settings{}
		}
		prev := h.st.Settings.FilesDir
		h.st.Settings.FilesDir = clean
		if err = h.st.save(); err != nil {
			h.st.Settings.FilesDir = prev
		}
		h.st.mu.Unlock()
	}
	if err != nil {
		_ = app.files.root.Close()
		return uerr(http.StatusInternalServerError, "保存设置失败: %v", err)
	}
	old.close()
	app.start()
	h.swap.Lock()
	h.cur.Store(app)
	h.swap.Unlock()
	// Requests that started on the old directory (this one included) finish
	// there; after the last of them the old directory is released.
	h.retiring.Add(1)
	go func() {
		defer h.retiring.Done()
		old.inflight.Wait()
		_ = old.files.root.Close()
	}()
	h.warn = ""
	log.Printf("存储位置已从 %s 改为 %s", old.files.dir, clean)
	return nil
}

func (h *Host) handleSettings(w http.ResponseWriter, r *http.Request) {
	app := h.cur.Load()
	h.st.mu.Lock()
	days := h.st.trashDays()
	username := ""
	if h.st.Admin != nil {
		username = h.st.Admin.Username
	}
	h.st.mu.Unlock()
	h.mu.Lock()
	warn := h.warn
	h.mu.Unlock()
	writeJSON(w, map[string]any{
		"username":  username,
		"trashDays": days,
		"filesDir":  app.files.dir,
		"warn":      warn,
		"system":    h.hook != nil, // the choice is also written to the system's own settings
		"example":   exampleDir(),
		"version":   version,
	})
}

func (h *Host) handleSetFilesDir(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dir      string `json:"dir"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	// The files directory decides what the web UI can read, so a stolen
	// session alone must not be able to move it.
	if !h.auth.confirmPassword(w, r, in.Password) {
		return
	}
	if err := h.SetFilesDir(in.Dir); err != nil {
		var ue *userErr
		if errors.As(err, &ue) {
			writeErr(w, ue.code, ue.msg)
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, map[string]any{"filesDir": h.cur.Load().files.dir})
}

// hostRoots lists where browsing for a directory can start.
func hostRoots() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for c := 'A'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			if _, err := os.Stat(d); err == nil {
				out = append(out, d)
			}
		}
		return out
	}
	out := []string{"/"}
	if b, err := os.ReadFile("/proc/mounts"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) > 1 && (strings.HasPrefix(f[1], "/mnt/") || strings.HasPrefix(f[1], "/media/")) {
				out = append(out, f[1])
			}
		}
	}
	return out
}

// handleHostDirs lists the sub-folders of a directory on the machine, for the
// "choose a folder" dialog. It shows folder names only, never files.
func (h *Host) handleHostDirs(w http.ResponseWriter, r *http.Request) {
	roots := hostRoots()
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if p == "" {
		writeJSON(w, map[string]any{"path": "", "parent": "", "dirs": []string{}, "roots": roots})
		return
	}
	if !filepath.IsAbs(p) {
		writeErr(w, http.StatusBadRequest, "请填写完整路径")
		return
	}
	p = filepath.Clean(p)
	des, err := os.ReadDir(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无法打开这个文件夹")
		return
	}
	dirs := []string{}
	for _, de := range des {
		name := de.Name()
		if !de.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		dirs = append(dirs, name)
		if len(dirs) >= 500 {
			break
		}
	}
	sort.Strings(dirs)
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	writeJSON(w, map[string]any{"path": p, "parent": parent, "dirs": dirs, "roots": roots, "sep": string(filepath.Separator)})
}
