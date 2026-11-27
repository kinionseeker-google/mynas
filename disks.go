package main

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// diskInfo is a storage volume as found on the machine.
type diskInfo struct {
	Path string // mount point or drive root
	FS   string
}

// Disks is the read-only "磁盘文件" area. It shows what is on the attached
// disks themselves, starting at their top folder. The cloud drive's own storage
// location is left out of it: that part is managed under "全部文件".
type Disks struct {
	files   *Files
	dataDir string // holds state.json; never shown
	mode    string // "" or "auto": find the disks; "off": none; otherwise a comma-separated list of folders
}

// File systems that never hold the user's files, even on a /dev device.
var pseudoFS = map[string]bool{"squashfs": true, "overlay": true, "tmpfs": true, "ramfs": true, "devtmpfs": true, "proc": true, "sysfs": true, "jffs2": true, "ubifs": true, "iso9660": true}

// Network file systems count as disks too: they have no /dev device.
var netFS = map[string]bool{"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "virtiofs": true, "9p": true}

// Where the system itself lives. A mount at or below one of these is not a data disk.
var systemTrees = []string{"/proc", "/sys", "/dev", "/etc", "/bin", "/sbin", "/lib", "/usr", "/boot", "/rom", "/overlay", "/run", "/var", "/tmp", "/snap"}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// parseMounts picks the data disks out of the text of /proc/mounts.
func parseMounts(text string) []diskInfo {
	seen := map[string]int{}
	var out []diskInfo
lines:
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		dev, mp, fst := f[0], path.Clean(unescapeMount(f[1])), f[2]
		if !strings.HasPrefix(mp, "/") || mp == "/" || pseudoFS[fst] {
			continue
		}
		if !strings.HasPrefix(dev, "/dev/") && !netFS[fst] {
			continue
		}
		for _, t := range systemTrees {
			if mp == t || strings.HasPrefix(mp, t+"/") {
				continue lines
			}
		}
		if i, ok := seen[mp]; ok { // mounted over: the later one is what is visible
			out[i].FS = fst
			continue
		}
		seen[mp] = len(out)
		out = append(out, diskInfo{Path: mp, FS: fst})
	}
	return out
}

// realPath resolves symbolic links; a path that cannot be resolved is only cleaned.
func realPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// list returns the disks offered right now. A disk is looked up again on every
// request, so one that was unplugged disappears and cannot be read by an old link.
func (d *Disks) list() []diskInfo {
	var found []diskInfo
	switch d.mode {
	case "off":
		return nil
	case "", "auto":
		found = systemDisks()
	default:
		for _, p := range strings.Split(d.mode, ",") {
			if p = strings.TrimSpace(p); p != "" && filepath.IsAbs(p) {
				found = append(found, diskInfo{Path: filepath.Clean(p)})
			}
		}
	}
	data := realPath(d.dataDir)
	out := found[:0]
	for _, k := range found {
		info, err := os.Stat(k.Path)
		if err != nil || !info.IsDir() {
			continue
		}
		// A disk that is the configuration folder, or lies inside it, is not shown at all.
		if within(realPath(k.Path), data) {
			continue
		}
		out = append(out, k)
	}
	return out
}

type diskView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	FS        string `json:"fs,omitempty"`
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	CloudRoot bool   `json:"cloudRoot"`           // the whole disk is the cloud drive's storage location
	CloudPath string `json:"cloudPath,omitempty"` // or the storage location is this folder on the disk
}

func diskName(p string) string {
	if v := filepath.VolumeName(p); v != "" && len(p) <= len(v)+1 {
		return v
	}
	return filepath.Base(p)
}

// childOf returns the slash-separated path of target below dir, when it is below it.
func childOf(dir, target string) (string, bool) {
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (d *Disks) view(k diskInfo) diskView {
	v := diskView{ID: k.Path, Name: diskName(k.Path), Path: k.Path, FS: k.FS}
	v.Total, v.Free, _ = diskUsage(k.Path)
	root, cloud := realPath(k.Path), realPath(d.files.dir)
	if within(root, cloud) {
		v.CloudRoot = true
	} else if rel, ok := childOf(root, cloud); ok {
		v.CloudPath = rel
	}
	return v
}

func (d *Disks) handleDisks(w http.ResponseWriter, r *http.Request) {
	out := []diskView{}
	for _, k := range d.list() {
		out = append(out, d.view(k))
	}
	writeJSON(w, map[string]any{"disks": out, "off": d.mode == "off"})
}

func cleanDiskRel(p string) string {
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// target is one checked request: the disk, the path on it and where that really is.
type diskTarget struct {
	disk diskInfo
	rel  string   // slash-separated, relative to the disk
	real string   // absolute, links resolved
	root *os.Root // the disk; the caller closes it
}

// resolve checks the disk and path of a request and opens the disk. It answers
// the request itself and returns nil when the place may not be read.
func (d *Disks) resolve(w http.ResponseWriter, r *http.Request) *diskTarget {
	id := r.URL.Query().Get("disk")
	var disk *diskInfo
	for _, k := range d.list() {
		if sameDir(k.Path, id) {
			disk = &k
			break
		}
	}
	if disk == nil {
		writeErr(w, http.StatusNotFound, "这块磁盘不存在，或者已经拔出")
		return nil
	}
	rel := cleanDiskRel(r.URL.Query().Get("path"))
	real, err := filepath.EvalSymlinks(filepath.Join(disk.Path, filepath.FromSlash(rel)))
	if err != nil {
		fileErr(w, err)
		return nil
	}
	// The configuration (accounts, saved passwords) is never readable from here,
	// not even through a link placed on the disk.
	if within(real, realPath(d.dataDir)) {
		fileErr(w, errHiddenPath)
		return nil
	}
	if within(real, realPath(d.files.dir)) {
		writeErr(w, http.StatusConflict, "这里是云盘的存储位置，请到“全部文件”里查看和管理")
		return nil
	}
	root, err := os.OpenRoot(disk.Path)
	if err != nil {
		fileErr(w, err)
		return nil
	}
	return &diskTarget{disk: *disk, rel: rel, real: real, root: root}
}

func sameName(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (d *Disks) handleList(w http.ResponseWriter, r *http.Request) {
	t := d.resolve(w, r)
	if t == nil {
		return
	}
	defer t.root.Close()
	dir, err := t.root.Open(osName(t.rel))
	if err != nil {
		fileErr(w, err)
		return
	}
	defer dir.Close()
	des, err := dir.ReadDir(-1)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "这不是一个文件夹")
		return
	}
	// Direct children that are special: the cloud drive's own folder is only
	// pointed at, the configuration folder is left out.
	cloud, isCloud := childOf(t.real, realPath(d.files.dir))
	data, isData := childOf(t.real, realPath(d.dataDir))
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		name := de.Name()
		if name == "System Volume Information" || name == "$RECYCLE.BIN" || (isData && sameName(name, data)) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// Show what a link points at, as long as it stays on this disk.
			if info, err = t.root.Stat(osName(path.Join(t.rel, name))); err != nil {
				continue
			}
		}
		e := entryOf(t.rel, info)
		e.Name, e.Path = name, path.Join(t.rel, name)
		if isCloud && sameName(name, cloud) && e.Dir {
			e.Cloud = true
		}
		entries = append(entries, e)
	}
	writeJSON(w, map[string]any{"path": t.rel, "entries": entries, "disk": d.view(t.disk)})
}

const (
	diskFindLimit = 500
	diskFindTime  = 8 * time.Second
)

// handleFind searches the names on one disk. A disk can hold far more files
// than the cloud drive's own folder, so the walk stops after a few seconds and
// says so instead of keeping a small router busy.
func (d *Disks) handleFind(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "请输入要搜索的名称")
		return
	}
	t := d.resolve(w, r)
	if t == nil {
		return
	}
	defer t.root.Close()
	skip := map[string]bool{}
	for _, p := range []string{d.files.dir, d.dataDir} {
		if rel, ok := childOf(t.real, realPath(p)); ok {
			skip[rel] = true
		}
	}
	start := osName(t.rel)
	deadline := time.Now().Add(diskFindTime)
	out := make([]Entry, 0, 64)
	truncated := false
	_ = fs.WalkDir(t.root.FS(), filepath.ToSlash(start), func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if de != nil && de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == "." || p == t.rel {
			return nil
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			truncated = true
			return fs.SkipAll
		}
		below := strings.TrimPrefix(strings.TrimPrefix(p, t.rel), "/")
		if de.IsDir() && skip[below] {
			return fs.SkipDir
		}
		if !strings.Contains(strings.ToLower(de.Name()), q) {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		e := entryOf(path.Dir(p), info)
		e.Path = p
		out = append(out, e)
		if len(out) >= diskFindLimit {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	writeJSON(w, map[string]any{"entries": out, "truncated": truncated, "disk": d.view(t.disk)})
}

func (d *Disks) handleDownload(w http.ResponseWriter, r *http.Request) {
	t := d.resolve(w, r)
	if t == nil {
		return
	}
	defer t.root.Close()
	if t.rel == "" {
		writeErr(w, http.StatusBadRequest, "请指定要下载的文件")
		return
	}
	fh, err := t.root.Open(osName(t.rel))
	if err != nil {
		fileErr(w, err)
		return
	}
	defer fh.Close()
	sendFile(w, r, fh)
}
