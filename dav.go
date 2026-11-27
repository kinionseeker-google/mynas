package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// Dav serves the files over WebDAV at /dav/, so that sync clients such as
// Obsidian's Remotely Save plugin can use the NAS as their remote storage.
//
// Clients log in with an access key, not with the admin account: a key is a
// user name plus a generated password, confined to one folder. The password
// ends up in plain text in the client's settings, so it must not be able to
// do more than reach that folder, and it must be revocable on its own.
type Dav struct {
	st    *Store
	files *Files

	mu    sync.Mutex
	locks map[string]webdav.LockSystem // by key id
	used  map[string]time.Time         // last successful login, by key id
}

func NewDav(st *Store, files *Files) *Dav {
	return &Dav{st: st, files: files, locks: map[string]webdav.LockSystem{}, used: map[string]time.Time{}}
}

const maxDavKeys = 50

var davUserRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

// The generated password has 160 random bits, so a plain SHA-256 is enough to
// store it and each request can be checked cheaply even on a slow router CPU.
func davHash(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	return sum[:]
}

func newDavPassword() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// authenticate returns a copy of the key matching the request's Basic credentials.
func (d *Dav) authenticate(r *http.Request) *DavKey {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return nil
	}
	hash := davHash(pass)
	d.st.mu.Lock()
	defer d.st.mu.Unlock()
	var found *DavKey
	for _, k := range d.st.DavKeys {
		userOK := subtle.ConstantTimeCompare([]byte(k.Username), []byte(user))
		passOK := subtle.ConstantTimeCompare(k.Hash, hash)
		if userOK&passOK == 1 {
			c := *k
			found = &c
		}
	}
	return found
}

func (d *Dav) lockSystem(id string) webdav.LockSystem {
	d.mu.Lock()
	defer d.mu.Unlock()
	ls := d.locks[id]
	if ls == nil {
		ls = webdav.NewMemLS()
		d.locks[id] = ls
	}
	d.used[id] = time.Now()
	return ls
}

func (d *Dav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := d.authenticate(r)
	if key == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="mynas", charset="UTF-8"`)
		http.Error(w, "需要 WebDAV 账号和访问密码", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, "PROPFIND":
	default:
		if key.ReadOnly {
			http.Error(w, "这个访问密码是只读的", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodPut {
		if err := d.files.spaceErr(r.ContentLength); err != nil {
			http.Error(w, err.Error(), http.StatusInsufficientStorage)
			return
		}
	}

	root := d.files.root
	if key.Dir != "" {
		// A sub-root confines every path of this request to the key's folder.
		sub, err := root.OpenRoot(osName(key.Dir))
		if errors.Is(err, fs.ErrNotExist) {
			// Do not quietly create an empty folder: after the files directory
			// was changed (or the folder deleted) a sync client would take the
			// empty remote for "everything was deleted" and delete its local
			// copies. Fail instead, until the folder is recreated on purpose.
			http.Error(w, "这个访问密码对应的文件夹在当前存储位置不存在，请到管理界面的“WebDAV 同步”页面处理", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, "无法打开这个访问密码对应的文件夹", http.StatusInternalServerError)
			return
		}
		defer sub.Close()
		root = sub
	}
	h := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: &davFS{root: root, base: key.Dir, files: d.files},
		LockSystem: d.lockSystem(key.ID),
	}
	h.ServeHTTP(w, r)
}

// davFS adapts an os.Root to webdav.FileSystem.
type davFS struct {
	root  *os.Root
	base  string // the root's path inside the files directory
	files *Files
}

// davName cleans a WebDAV path into one relative to the root ("" is the root
// itself). The recycle bin and unfinished transfers do not exist for clients.
func davName(name string) (string, error) {
	p := strings.TrimPrefix(path.Clean("/"+name), "/")
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if hidden(seg) {
			return "", fs.ErrNotExist
		}
	}
	return p, nil
}

func (d *davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	p, err := davName(name)
	if err != nil {
		return err
	}
	if p == "" {
		return fs.ErrExist
	}
	return d.root.Mkdir(osName(p), 0o755)
}

func (d *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	p, err := davName(name)
	if err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(osName(p), flag, 0o644)
	if err != nil {
		return nil, err
	}
	return davFile{f}, nil
}

// RemoveAll moves the file to the recycle bin instead of deleting it: a sync
// that goes wrong can then be undone from the web UI.
func (d *davFS) RemoveAll(ctx context.Context, name string) error {
	p, err := davName(name)
	if err != nil {
		return err
	}
	if p == "" {
		return fs.ErrPermission // never remove the shared folder itself
	}
	err = d.files.moveToTrash(path.Join(d.base, p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (d *davFS) Rename(ctx context.Context, oldName, newName string) error {
	o, err := davName(oldName)
	if err != nil {
		return err
	}
	n, err := davName(newName)
	if err != nil {
		return fs.ErrPermission
	}
	if o == "" || n == "" {
		return fs.ErrPermission
	}
	return d.root.Rename(osName(o), osName(n))
}

func (d *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	p, err := davName(name)
	if err != nil {
		return nil, err
	}
	return d.root.Stat(osName(p))
}

// davFile hides the recycle bin and unfinished transfers from directory listings.
type davFile struct{ *os.File }

func (f davFile) Readdir(n int) ([]fs.FileInfo, error) {
	infos, err := f.File.Readdir(n)
	kept := infos[:0]
	for _, info := range infos {
		if !hidden(info.Name()) {
			kept = append(kept, info)
		}
	}
	return kept, err
}

// ---------- managing the access keys (admin API) ----------

type davKeyView struct {
	ID       string     `json:"id"`
	Username string     `json:"username"`
	Space    string     `json:"space"` // set when the key opens a sync space
	Dir      string     `json:"dir"`   // otherwise the folder inside the files directory
	ReadOnly bool       `json:"readOnly"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed"`
	Missing  bool       `json:"missing"` // its folder does not exist in the files directory in use
}

// handleInit creates the (empty) folder of a key whose folder is missing, for
// example after the files directory was changed.
func (d *Dav) handleInit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.st.mu.Lock()
	dir, found := "", false
	for _, k := range d.st.DavKeys {
		if k.ID == id {
			dir, found = k.Dir, true
		}
	}
	d.st.mu.Unlock()
	if !found {
		writeErr(w, http.StatusNotFound, "访问密码不存在")
		return
	}
	if err := d.files.root.MkdirAll(osName(dir), 0o755); err != nil {
		fileErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (d *Dav) view(k *DavKey) davKeyView {
	v := davKeyView{ID: k.ID, Username: k.Username, Dir: k.Dir, ReadOnly: k.ReadOnly, Created: k.Created}
	if name, ok := spaceOf(k.Dir); ok {
		v.Space, v.Dir = name, ""
	}
	if k.Dir != "" {
		if info, err := d.files.root.Stat(osName(k.Dir)); err != nil || !info.IsDir() {
			v.Missing = true
		}
	}
	d.mu.Lock()
	if t, ok := d.used[k.ID]; ok {
		v.LastUsed = &t
	}
	d.mu.Unlock()
	return v
}

func (d *Dav) handleList(w http.ResponseWriter, r *http.Request) {
	d.st.mu.Lock()
	out := make([]davKeyView, 0, len(d.st.DavKeys))
	for _, k := range d.st.DavKeys {
		out = append(out, d.view(k))
	}
	d.st.mu.Unlock()
	writeJSON(w, map[string]any{"keys": out})
}

func (d *Dav) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Space    string `json:"space"` // a sync space, kept apart from the user's files
		Dir      string `json:"dir"`   // or a folder inside the files directory
		ReadOnly bool   `json:"readOnly"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if !davUserRe.MatchString(in.Username) {
		writeErr(w, http.StatusBadRequest, "账号只能用字母、数字和 . _ -，以字母或数字开头，最多 32 个字符")
		return
	}
	var dir string
	if in.Space = strings.TrimSpace(in.Space); in.Space != "" {
		if !validName(in.Space) {
			writeErr(w, http.StatusBadRequest, "同步空间名称不合法")
			return
		}
		dir = syncDir + "/" + in.Space
	} else {
		clean, err := cleanRel(in.Dir)
		if err != nil {
			fileErr(w, err)
			return
		}
		for _, seg := range strings.Split(clean, "/") {
			if clean != "" && !validName(seg) {
				writeErr(w, http.StatusBadRequest, "文件夹名称不合法")
				return
			}
		}
		dir = clean
	}
	if err := d.files.root.MkdirAll(osName(dir), 0o755); err != nil {
		fileErr(w, err)
		return
	}
	if info, err := d.files.root.Stat(osName(dir)); err != nil || !info.IsDir() {
		writeErr(w, http.StatusBadRequest, "这个位置不是文件夹")
		return
	}

	password := newDavPassword()
	key := &DavKey{Username: in.Username, Hash: davHash(password), Dir: dir, ReadOnly: in.ReadOnly, Created: time.Now()}
	d.st.mu.Lock()
	if len(d.st.DavKeys) >= maxDavKeys {
		d.st.mu.Unlock()
		writeErr(w, http.StatusConflict, "访问密码数量已达上限，请先移除不用的")
		return
	}
	for _, k := range d.st.DavKeys {
		if k.Username == in.Username {
			d.st.mu.Unlock()
			writeErr(w, http.StatusConflict, "这个账号已经存在，请换一个")
			return
		}
	}
	key.ID = d.st.nextID("k")
	d.st.DavKeys = append(d.st.DavKeys, key)
	err := d.st.save()
	d.st.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	// The password is returned this once; only its hash is kept.
	writeJSON(w, map[string]any{"key": d.view(key), "password": password})
}

// handleNewPassword replaces a key's password. The old one stops working at
// once; the account and its folder stay as they are.
func (d *Dav) handleNewPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	password := newDavPassword()
	d.st.mu.Lock()
	for _, k := range d.st.DavKeys {
		if k.ID != id {
			continue
		}
		old := k.Hash
		k.Hash = davHash(password)
		if err := d.st.save(); err != nil {
			k.Hash = old
			d.st.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		c := *k
		d.st.mu.Unlock()
		writeJSON(w, map[string]any{"key": d.view(&c), "password": password})
		return
	}
	d.st.mu.Unlock()
	writeErr(w, http.StatusNotFound, "访问密码不存在")
}

func (d *Dav) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.st.mu.Lock()
	defer d.st.mu.Unlock()
	for i, k := range d.st.DavKeys {
		if k.ID == id {
			d.st.DavKeys = append(d.st.DavKeys[:i], d.st.DavKeys[i+1:]...)
			if err := d.st.save(); err != nil {
				writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
				return
			}
			d.mu.Lock()
			delete(d.locks, id)
			delete(d.used, id)
			d.mu.Unlock()
			writeJSON(w, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "访问密码不存在")
}
