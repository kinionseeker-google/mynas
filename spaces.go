package main

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// Sync spaces keep the data of sync clients (an Obsidian vault, for example)
// apart from the user's own files. A space is a folder under .mynas-sync in
// the files directory: it does not show up in "全部文件", the type categories
// or search, and is browsed from the WebDAV page instead. The file API reaches
// a space through its ?space= parameter (see Files.scope).

type spaceView struct {
	Name    string   `json:"name"`
	Keys    []string `json:"keys"`    // user names of the access keys that open this space
	Entries int      `json:"entries"` // items at the top level
	Mtime   int64    `json:"mtime"`
}

// spaceOf returns the space a WebDAV key directory points at, if it is one.
func spaceOf(dir string) (string, bool) {
	name, ok := strings.CutPrefix(dir, syncDir+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

func (d *Dav) handleSpaces(w http.ResponseWriter, r *http.Request) {
	users := map[string][]string{}
	d.st.mu.Lock()
	for _, k := range d.st.DavKeys {
		if name, ok := spaceOf(k.Dir); ok {
			users[name] = append(users[name], k.Username)
		}
	}
	d.st.mu.Unlock()

	out := []spaceView{}
	if dir, err := d.files.root.Open(syncDir); err == nil {
		des, _ := dir.ReadDir(-1)
		dir.Close()
		for _, de := range des {
			if !de.IsDir() || hidden(de.Name()) {
				continue
			}
			v := spaceView{Name: de.Name(), Keys: users[de.Name()]}
			if v.Keys == nil {
				v.Keys = []string{}
			}
			if info, err := de.Info(); err == nil {
				v.Mtime = info.ModTime().UnixMilli()
			}
			if sub, err := d.files.root.Open(osName(syncDir + "/" + de.Name())); err == nil {
				names, _ := sub.Readdirnames(-1)
				sub.Close()
				for _, n := range names {
					if !hidden(n) {
						v.Entries++
					}
				}
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"spaces": out})
}

// handleSpaceDelete moves a space that no access key uses any more to the recycle bin.
func (d *Dav) handleSpaceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "同步空间名称不合法")
		return
	}
	d.st.mu.Lock()
	for _, k := range d.st.DavKeys {
		if n, ok := spaceOf(k.Dir); ok && n == name {
			d.st.mu.Unlock()
			writeErr(w, http.StatusConflict, "还有访问密码在使用这个同步空间，请先移除它们")
			return
		}
	}
	d.st.mu.Unlock()
	if err := d.files.moveToTrash(syncDir + "/" + name); err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleToSpace turns the folder of an access key into a sync space: the
// folder moves out of "全部文件" and every key that pointed at it follows, so
// the sync clients keep working without any change on their side.
func (d *Dav) handleToSpace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.st.mu.Lock()
	old, found := "", false
	for _, k := range d.st.DavKeys {
		if k.ID == id {
			old, found = k.Dir, true
		}
	}
	d.st.mu.Unlock()
	if !found {
		writeErr(w, http.StatusNotFound, "访问密码不存在")
		return
	}
	if old == "" {
		writeErr(w, http.StatusBadRequest, "可以进入全部文件的访问密码不能移到同步空间")
		return
	}
	if _, ok := spaceOf(old); ok {
		writeErr(w, http.StatusBadRequest, "它已经在同步空间里了")
		return
	}

	root := d.files.root
	if err := root.MkdirAll(syncDir, 0o755); err != nil {
		fileErr(w, err)
		return
	}
	name := d.files.uniqueName(syncDir, sanitizeName(path.Base(old)))
	dst := syncDir + "/" + name
	err := root.Rename(osName(old), osName(dst))
	if errors.Is(err, fs.ErrNotExist) {
		err = root.MkdirAll(osName(dst), 0o755) // the folder was never created; start empty
	}
	if err != nil {
		fileErr(w, err)
		return
	}

	d.st.mu.Lock()
	for _, k := range d.st.DavKeys {
		if k.Dir == old {
			k.Dir = dst
		}
	}
	err = d.st.save()
	d.st.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"space": name})
}
