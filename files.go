package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	partSuffix = ".mynas-part" // unfinished uploads and downloads
	trashDir   = ".trash"
	syncDir    = ".mynas-sync" // holds the sync spaces, see spaces.go
)

// Entry is one file or folder as the web UI sees it.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // slash-separated, relative to the files root
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // unix milliseconds
	Type  string `json:"type"`
}

// Files serves the local file tree. All disk access goes through os.Root,
// so a request can never reach outside the files directory.
type Files struct {
	st      *Store
	root    *os.Root
	dir     string
	minFree uint64 // bytes that must stay free on the volume

	// trashSeen holds, for items deleted by this process, the deletion time
	// with its monotonic clock reading. Guarded by st.mu.
	trashSeen map[string]time.Time

	stop chan struct{} // closed when this directory is no longer the one in use
}

// spaceErr reports an error when writing need more bytes would leave less than
// minFree on the volume. It protects small router flash from being filled up.
func (f *Files) spaceErr(need int64) error {
	_, free, err := diskUsage(f.dir)
	if err != nil {
		return nil // cannot tell; do not block
	}
	if need < 0 {
		need = 0
	}
	if free < f.minFree+uint64(need) {
		return fmt.Errorf("磁盘剩余空间不足（剩余 %d MB，至少保留 %d MB）。请清理空间，或把文件目录改到更大的磁盘", free>>20, f.minFree>>20)
	}
	return nil
}

var errHiddenPath = errors.New("不能访问这个位置")

// cleanRel normalises an API path to a slash-separated path relative to the root ("" is the root).
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, seg := range strings.Split(p, "/") {
		if hidden(seg) {
			return "", errHiddenPath
		}
	}
	return p, nil
}

func osName(rel string) string {
	if rel == "" {
		return "."
	}
	return filepath.FromSlash(rel)
}

func hidden(name string) bool {
	return name == trashDir || name == syncDir || strings.HasSuffix(name, partSuffix)
}

// scope returns the directory a file request works in: "" for the user's own
// files, or the folder of the sync space named by ?space=. Paths in the
// request and in the reply are relative to it.
func (f *Files) scope(r *http.Request) (string, error) {
	name := r.URL.Query().Get("space")
	if name == "" {
		return "", nil
	}
	if !validName(name) {
		return "", errHiddenPath
	}
	return syncDir + "/" + name, nil
}

func validName(n string) bool {
	if n == "" || n == "." || n == ".." || len(n) > 255 || hidden(n) {
		return false
	}
	if strings.ContainsAny(n, `/\:*?"<>|`) || strings.HasSuffix(n, " ") || strings.HasSuffix(n, ".") {
		return false
	}
	for _, r := range n {
		if r < 0x20 {
			return false
		}
	}
	return true
}

// sanitizeName turns an arbitrary remote file name into one validName accepts.
func sanitizeName(n string) string {
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, n)
	n = strings.TrimRight(strings.TrimSpace(n), ". ")
	if len(n) > 200 {
		ext := path.Ext(n)
		if len(ext) > 20 {
			ext = ""
		}
		n = strings.ToValidUTF8(n[:200-len(ext)], "") + ext
	}
	if !validName(n) {
		return "download"
	}
	return n
}

var typeByExt = map[string]string{}

func init() {
	for typ, exts := range map[string]string{
		"image": "jpg jpeg png gif webp bmp heic heif svg tif tiff",
		"video": "mp4 mkv mov avi webm m4v ts flv wmv rmvb",
		"audio": "mp3 flac wav aac m4a ogg opus wma ape",
		"doc":   "pdf doc docx xls xlsx ppt pptx txt md csv rtf epub odt ods",
	} {
		for _, e := range strings.Fields(exts) {
			typeByExt["."+e] = typ
		}
	}
}

func fileType(name string) string {
	if t, ok := typeByExt[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "other"
}

func entryOf(dirRel string, info fs.FileInfo) Entry {
	e := Entry{Name: info.Name(), Path: path.Join(dirRel, info.Name()), Dir: info.IsDir(), Mtime: info.ModTime().UnixMilli()}
	if e.Dir {
		e.Type = "folder"
	} else {
		e.Size = info.Size()
		e.Type = fileType(e.Name)
	}
	return e
}

func fileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errHiddenPath):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, fs.ErrNotExist):
		writeErr(w, http.StatusNotFound, "文件或文件夹不存在")
	case errors.Is(err, fs.ErrExist):
		writeErr(w, http.StatusConflict, "同名文件或文件夹已存在")
	case errors.Is(err, fs.ErrPermission):
		writeErr(w, http.StatusForbidden, "没有权限执行这个操作")
	default:
		writeErr(w, http.StatusInternalServerError, "操作失败: "+err.Error())
	}
}

func (f *Files) exists(rel string) bool {
	_, err := f.root.Lstat(osName(rel))
	return err == nil
}

// uniqueName returns name, or "name (n).ext" when name is taken inside dirRel.
func (f *Files) uniqueName(dirRel, name string) string {
	if !f.exists(path.Join(dirRel, name)) {
		return name
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if !f.exists(path.Join(dirRel, cand)) {
			return cand
		}
	}
}

func (f *Files) handleList(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil {
		fileErr(w, err)
		return
	}
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	d, err := f.root.Open(osName(path.Join(base, rel)))
	if err != nil {
		fileErr(w, err)
		return
	}
	defer d.Close()
	des, err := d.ReadDir(-1)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "这不是一个文件夹")
		return
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		if hidden(de.Name()) {
			continue
		}
		if info, err := de.Info(); err == nil {
			entries = append(entries, entryOf(rel, info))
		}
	}
	writeJSON(w, map[string]any{"path": rel, "entries": entries})
}

// handleFind walks the tree for search, the type categories and the recent list.
// A full walk is fine at home scale; an index replaces it later.
func (f *Files) handleFind(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	typ := r.URL.Query().Get("type")
	recent := r.URL.Query().Get("recent") == "1"
	limit := 1000
	if recent {
		limit = 50000
	}
	out := make([]Entry, 0, 64)
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	start := "."
	if base != "" {
		start = base
	}
	_ = fs.WalkDir(f.root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == start {
			return nil
		}
		name := d.Name()
		if hidden(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if (recent || typ != "") && d.IsDir() {
			return nil
		}
		if typ != "" && fileType(name) != typ {
			return nil
		}
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, entryOf(path.Dir(strings.TrimPrefix(p, base+"/")), info))
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if recent {
		sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
		if len(out) > 60 {
			out = out[:60]
		}
	}
	writeJSON(w, map[string]any{"entries": out})
}

// inlineSafe lists the types the browser may render directly; everything else downloads.
func inlineSafe(name string) bool {
	if strings.EqualFold(path.Ext(name), ".svg") {
		return false
	}
	switch fileType(name) {
	case "image", "video", "audio":
		return true
	}
	return false
}

func contentDisposition(kind, name string) string {
	return kind + "; filename*=UTF-8''" + url.PathEscape(name)
}

func (f *Files) handleDownload(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		writeErr(w, http.StatusBadRequest, "请指定要下载的文件")
		return
	}
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	fh, err := f.root.Open(osName(path.Join(base, rel)))
	if err != nil {
		fileErr(w, err)
		return
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		fileErr(w, err)
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "暂不支持下载整个文件夹")
		return
	}
	kind := "attachment"
	if r.URL.Query().Get("inline") == "1" && inlineSafe(info.Name()) {
		kind = "inline"
	}
	w.Header().Set("Content-Disposition", contentDisposition(kind, info.Name()))
	// A file opened as a page must never run scripts with the NAS session.
	w.Header().Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, info.Name(), info.ModTime(), fh)
}

func (f *Files) handleUpload(w http.ResponseWriter, r *http.Request) {
	dirRel, err := cleanRel(r.URL.Query().Get("dir"))
	if err != nil {
		fileErr(w, err)
		return
	}
	name := r.URL.Query().Get("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "文件名不合法")
		return
	}
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	full := path.Join(base, dirRel) // where the file really goes; dirRel is what the client sees
	if info, err := f.root.Stat(osName(full)); err != nil || !info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标文件夹不存在")
		return
	}
	if err := f.spaceErr(r.ContentLength); err != nil {
		writeErr(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	tmp := path.Join(full, "."+randomHex(8)+partSuffix)
	out, err := f.root.OpenFile(osName(tmp), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fileErr(w, err)
		return
	}
	_, err = io.Copy(out, r.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = f.root.Remove(osName(tmp))
		writeErr(w, http.StatusBadRequest, "上传中断: "+err.Error())
		return
	}
	final := path.Join(full, f.uniqueName(full, name))
	if err := f.root.Rename(osName(tmp), osName(final)); err != nil {
		_ = f.root.Remove(osName(tmp))
		fileErr(w, err)
		return
	}
	info, err := f.root.Stat(osName(final))
	if err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, entryOf(dirRel, info))
}

func (f *Files) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var in struct{ Dir, Name string }
	if !readJSON(w, r, &in) {
		return
	}
	dirRel, err := cleanRel(in.Dir)
	if err != nil {
		fileErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validName(in.Name) {
		writeErr(w, http.StatusBadRequest, "文件夹名称不合法")
		return
	}
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	if err := f.root.Mkdir(osName(path.Join(base, dirRel, in.Name)), 0o755); err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (f *Files) handleRename(w http.ResponseWriter, r *http.Request) {
	var in struct{ Path, Name string }
	if !readJSON(w, r, &in) {
		return
	}
	rel, err := cleanRel(in.Path)
	if err != nil || rel == "" {
		writeErr(w, http.StatusBadRequest, "请指定要重命名的文件")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validName(in.Name) {
		writeErr(w, http.StatusBadRequest, "名称不合法")
		return
	}
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	rel = path.Join(base, rel)
	dst := path.Join(path.Dir(rel), in.Name)
	if dst == rel {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	// A case-only rename keeps the same file on case-insensitive disks, so allow it.
	if f.exists(dst) && !strings.EqualFold(dst, rel) {
		writeErr(w, http.StatusConflict, "同名文件或文件夹已存在")
		return
	}
	if err := f.root.Rename(osName(rel), osName(dst)); err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// moveToTrash moves rel into .trash and records where it came from.
func (f *Files) moveToTrash(rel string) error {
	info, err := f.root.Lstat(osName(rel))
	if err != nil {
		return err
	}
	if err := f.root.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}
	f.st.mu.Lock()
	id := f.st.nextID("t")
	f.st.mu.Unlock()
	if err := f.root.Rename(osName(rel), osName(trashDir+"/"+id)); err != nil {
		return err
	}
	now := time.Now()
	item := &TrashItem{ID: id, Name: info.Name(), OrigPath: rel, Dir: info.IsDir(), DeletedAt: now.Round(0), Home: f.dir}
	if !item.Dir {
		item.Size = info.Size()
	}
	f.st.mu.Lock()
	f.st.Trash = append(f.st.Trash, item)
	f.trashSeen[id] = now
	err = f.st.save()
	f.st.mu.Unlock()
	return err
}

func (f *Files) handleDelete(w http.ResponseWriter, r *http.Request) {
	var in struct{ Paths []string }
	if !readJSON(w, r, &in) {
		return
	}
	base, err := f.scope(r)
	if err != nil {
		fileErr(w, err)
		return
	}
	done := 0
	var firstErr error
	for _, p := range in.Paths {
		rel, err := cleanRel(p)
		if err == nil && rel == "" {
			err = errHiddenPath
		}
		if err == nil {
			err = f.moveToTrash(path.Join(base, rel))
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		done++
	}
	if done == 0 && firstErr != nil {
		fileErr(w, firstErr)
		return
	}
	writeJSON(w, map[string]any{"deleted": done})
}

func (f *Files) handleTrashList(w http.ResponseWriter, r *http.Request) {
	f.st.mu.Lock()
	days := f.st.trashDays()
	items := make([]TrashItem, 0, len(f.st.Trash))
	for _, t := range f.st.Trash {
		if sameDir(t.Home, f.dir) { // items of another files directory are kept but not shown
			items = append(items, *t)
		}
	}
	f.st.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].DeletedAt.After(items[j].DeletedAt) })
	writeJSON(w, map[string]any{"items": items, "trashDays": days})
}

// takeTrash removes the records with the given ids from the state and returns them.
func (f *Files) takeTrash(ids []string) ([]*TrashItem, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	var taken, keep []*TrashItem
	for _, t := range f.st.Trash {
		if want[t.ID] && sameDir(t.Home, f.dir) {
			taken = append(taken, t)
			delete(f.trashSeen, t.ID)
		} else {
			keep = append(keep, t)
		}
	}
	f.st.Trash = keep
	return taken, f.st.save()
}

func (f *Files) putBackTrash(items []*TrashItem) {
	if len(items) == 0 {
		return
	}
	f.st.mu.Lock()
	f.st.Trash = append(f.st.Trash, items...)
	_ = f.st.save()
	f.st.mu.Unlock()
}

func (f *Files) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	var in struct{ IDs []string }
	if !readJSON(w, r, &in) {
		return
	}
	items, err := f.takeTrash(in.IDs)
	if err != nil {
		fileErr(w, err)
		return
	}
	var failed []*TrashItem
	for _, t := range items {
		parent := path.Dir(t.OrigPath)
		if parent == "." {
			parent = ""
		}
		err := f.root.MkdirAll(osName(parent), 0o755)
		if err == nil {
			dst := path.Join(parent, f.uniqueName(parent, t.Name))
			err = f.root.Rename(osName(trashDir+"/"+t.ID), osName(dst))
		}
		if err != nil {
			failed = append(failed, t)
		}
	}
	f.putBackTrash(failed)
	if len(failed) > 0 && len(failed) == len(items) {
		writeErr(w, http.StatusInternalServerError, "还原失败")
		return
	}
	writeJSON(w, map[string]any{"restored": len(items) - len(failed)})
}

// purge deletes the given trash items for good and returns how many were removed.
func (f *Files) purge(ids []string) (int, error) {
	items, err := f.takeTrash(ids)
	if err != nil {
		return 0, err
	}
	var failed []*TrashItem
	for _, t := range items {
		if err := f.root.RemoveAll(osName(trashDir + "/" + t.ID)); err != nil {
			failed = append(failed, t)
		}
	}
	f.putBackTrash(failed)
	return len(items) - len(failed), nil
}

func (f *Files) handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	var in struct{ IDs []string }
	if !readJSON(w, r, &in) {
		return
	}
	purged, err := f.purge(in.IDs)
	if err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"purged": purged})
}

// expired reports whether a trash item deleted at deletedAt is past the retention.
func expired(deletedAt, now time.Time, days int) bool {
	return days > 0 && now.Sub(deletedAt) >= time.Duration(days)*24*time.Hour
}

// purgeExpired deletes the trash items older than the retention setting and
// returns how many it removed.
//
// Routers often have no battery-backed clock: right after boot the time can be
// weeks behind until NTP corrects it. A file deleted in that window would get a
// stale timestamp and look expired as soon as the clock is fixed. For items
// deleted by this process the real age is known from the monotonic clock, so
// their stored time is corrected before the retention is applied.
func (f *Files) purgeExpired(now time.Time) int {
	f.st.mu.Lock()
	days := f.st.trashDays()
	var ids []string
	corrected := false
	for _, t := range f.st.Trash {
		if !sameDir(t.Home, f.dir) {
			continue // lives in another files directory; leave it alone
		}
		if seen, ok := f.trashSeen[t.ID]; ok {
			real := now.Add(-now.Sub(seen)).Round(0)
			if d := real.Sub(t.DeletedAt); d > time.Hour || d < -time.Hour {
				t.DeletedAt = real
				corrected = true
			}
		}
		if expired(t.DeletedAt, now, days) {
			ids = append(ids, t.ID)
		}
	}
	if corrected {
		_ = f.st.save()
	}
	f.st.mu.Unlock()
	if len(ids) == 0 {
		return 0
	}
	n, err := f.purge(ids)
	if err != nil {
		log.Printf("回收站自动清除失败: %v", err)
	}
	if n > 0 {
		log.Printf("回收站自动清除了 %d 项（保留 %d 天）", n, days)
	}
	return n
}

// trashLoop applies the retention shortly after start and then once an hour.
func (f *Files) trashLoop() {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			f.purgeExpired(time.Now())
			t.Reset(time.Hour)
		}
	}
}

func (f *Files) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TrashDays *int `json:"trashDays"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.TrashDays == nil || *in.TrashDays < 0 || *in.TrashDays > maxTrashDays {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("自动清除的天数要在 0 到 %d 之间，0 表示不自动清除", maxTrashDays))
		return
	}
	f.st.mu.Lock()
	if f.st.Settings == nil {
		f.st.Settings = &Settings{}
	}
	days := *in.TrashDays
	f.st.Settings.TrashDays = &days
	err := f.st.save()
	f.st.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	// A shorter retention takes effect at once.
	purged := f.purgeExpired(time.Now())
	writeJSON(w, map[string]any{"trashDays": *in.TrashDays, "purged": purged})
}

func (f *Files) handleStorage(w http.ResponseWriter, r *http.Request) {
	total, free, err := diskUsage(f.dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取磁盘容量失败")
		return
	}
	// dir tells the admin which directory "全部文件" really is.
	writeJSON(w, map[string]any{"total": total, "free": free, "used": total - free, "dir": f.dir})
}
