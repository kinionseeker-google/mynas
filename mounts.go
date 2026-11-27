package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// ---------- WebDAV client ----------

const propfindBody = `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:displayname/><D:getcontentlength/><D:getlastmodified/><D:resourcetype/></D:prop></D:propfind>`

type davMulti struct {
	Responses []davResp `xml:"DAV: response"`
}

type davResp struct {
	Href      string        `xml:"DAV: href"`
	Propstats []davPropstat `xml:"DAV: propstat"`
}

type davPropstat struct {
	Status string  `xml:"DAV: status"`
	Prop   davProp `xml:"DAV: prop"`
}

type davProp struct {
	Length   string `xml:"DAV: getcontentlength"`
	Modified string `xml:"DAV: getlastmodified"`
	Type     struct {
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
}

type davClient struct {
	base *url.URL
	user string
	pass string
	tr   *http.Transport
	hc   *http.Client
}

func newDavClient(m Mount) (*davClient, error) {
	u, err := parseHTTPURL(m.URL)
	if err != nil {
		return nil, err
	}
	tr := insecureTransport(m.Insecure)
	return &davClient{base: u, user: m.Username, pass: m.Password, tr: tr, hc: &http.Client{Transport: tr}}, nil
}

func (c *davClient) close() { c.tr.CloseIdleConnections() }

// cleanDavPath returns an absolute, traversal-free path inside the mount ("/" is its root).
func cleanDavPath(p string) string {
	return path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
}

func (c *davClient) urlFor(p string) string {
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + p
	u.RawPath = ""
	return u.String()
}

func (c *davClient) do(req *http.Request) (*http.Response, error) {
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	req.Header.Set("User-Agent", "mynas/0.1")
	return c.hc.Do(req)
}

func davStatusErr(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("账号或密码不正确，或没有访问权限")
	case http.StatusNotFound:
		return errors.New("目录或文件不存在")
	}
	return fmt.Errorf("服务器返回 %s", resp.Status)
}

// list returns the children of directory p.
func (c *davClient) list(ctx context.Context, p string) ([]Entry, error) {
	p = cleanDavPath(p)
	reqPath := p
	if reqPath != "/" {
		reqPath += "/"
	}
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", c.urlFor(reqPath), strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		if resp.StatusCode < 300 || resp.StatusCode == http.StatusMethodNotAllowed {
			return nil, errors.New("这个地址不是 WebDAV 服务")
		}
		return nil, davStatusErr(resp)
	}
	var ms davMulti
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&ms); err != nil {
		return nil, errors.New("无法解析服务器返回的目录内容")
	}
	self := strings.TrimSuffix(strings.TrimSuffix(c.base.Path, "/")+p, "/")
	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		hu, err := url.Parse(strings.TrimSpace(r.Href))
		if err != nil {
			continue
		}
		hp := strings.TrimSuffix(hu.Path, "/")
		if hp == self {
			continue
		}
		name := path.Base(hp)
		if name == "" || name == "." || name == "/" {
			continue
		}
		var prop davProp
		for i, ps := range r.Propstats {
			if i == 0 || strings.Contains(ps.Status, " 200") {
				prop = ps.Prop
			}
		}
		e := Entry{Name: name, Path: strings.TrimPrefix(path.Join(p, name), "/"), Dir: prop.Type.Collection != nil}
		if t, err := http.ParseTime(strings.TrimSpace(prop.Modified)); err == nil {
			e.Mtime = t.UnixMilli()
		}
		if e.Dir {
			e.Type = "folder"
		} else {
			e.Size, _ = strconv.ParseInt(strings.TrimSpace(prop.Length), 10, 64)
			e.Type = fileType(name)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// get fetches file p, optionally from a byte range.
func (c *davClient) get(ctx context.Context, p, rangeHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.urlFor(cleanDavPath(p)), nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return c.do(req)
}

// ---------- API ----------

// Mounts manages external drives. They are read-only for now: browse, download, copy into the NAS.
type Mounts struct {
	st *Store
	dl *Downloads
}

type mountView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Username    string `json:"username"`
	Insecure    bool   `json:"insecure"`
	HasPassword bool   `json:"hasPassword"`
}

func viewOf(m *Mount) mountView {
	return mountView{ID: m.ID, Name: m.Name, Type: m.Type, URL: m.URL, Username: m.Username, Insecure: m.Insecure, HasPassword: m.Password != ""}
}

func normalizeMount(m *Mount) error {
	m.Name = strings.TrimSpace(m.Name)
	m.Username = strings.TrimSpace(m.Username)
	if m.Name == "" || len([]rune(m.Name)) > 30 {
		return errors.New("请填写名称（不超过 30 个字）")
	}
	if m.Type == "" {
		m.Type = "webdav"
	}
	if m.Type != "webdav" {
		return errors.New("目前只支持 WebDAV")
	}
	u, err := parseHTTPURL(m.URL)
	if err != nil {
		return errors.New("WebDAV 地址需要以 http:// 或 https:// 开头，且不要包含账号密码")
	}
	m.URL = u.String()
	return nil
}

// get returns a copy of the mount. It locks the store itself.
func (ms *Mounts) get(id string) (Mount, bool) {
	ms.st.mu.Lock()
	defer ms.st.mu.Unlock()
	for _, m := range ms.st.Mounts {
		if m.ID == id {
			return *m, true
		}
	}
	return Mount{}, false
}

func (ms *Mounts) handleList(w http.ResponseWriter, r *http.Request) {
	ms.st.mu.Lock()
	out := make([]mountView, 0, len(ms.st.Mounts))
	for _, m := range ms.st.Mounts {
		out = append(out, viewOf(m))
	}
	ms.st.mu.Unlock()
	writeJSON(w, map[string]any{"mounts": out})
}

func (ms *Mounts) handleCreate(w http.ResponseWriter, r *http.Request) {
	var m Mount
	if !readJSON(w, r, &m) {
		return
	}
	if err := normalizeMount(&m); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ms.st.mu.Lock()
	m.ID = ms.st.nextID("m")
	ms.st.Mounts = append(ms.st.Mounts, &m)
	err := ms.st.save()
	ms.st.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, viewOf(&m))
}

func (ms *Mounts) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in Mount
	if !readJSON(w, r, &in) {
		return
	}
	if err := normalizeMount(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	ms.st.mu.Lock()
	defer ms.st.mu.Unlock()
	for _, m := range ms.st.Mounts {
		if m.ID != id {
			continue
		}
		m.Name, m.URL, m.Username, m.Insecure = in.Name, in.URL, in.Username, in.Insecure
		if in.Password != "" { // an empty password keeps the saved one
			m.Password = in.Password
		}
		if err := ms.st.save(); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, viewOf(m))
		return
	}
	writeErr(w, http.StatusNotFound, "网盘不存在")
}

func (ms *Mounts) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ms.st.mu.Lock()
	defer ms.st.mu.Unlock()
	for i, m := range ms.st.Mounts {
		if m.ID == id {
			ms.st.Mounts = append(ms.st.Mounts[:i], ms.st.Mounts[i+1:]...)
			if err := ms.st.save(); err != nil {
				writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "网盘不存在")
}

// handleTest tries the connection before (or after) saving. With an id and an
// empty password it reuses the saved password.
func (ms *Mounts) handleTest(w http.ResponseWriter, r *http.Request) {
	var in Mount
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == "" {
		in.Name = "test"
	}
	if err := normalizeMount(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.ID != "" && in.Password == "" {
		if saved, ok := ms.get(in.ID); ok {
			in.Password = saved.Password
		}
	}
	c, err := newDavClient(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	entries, err := c.list(ctx, "/")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "count": len(entries)})
}

func (ms *Mounts) client(w http.ResponseWriter, r *http.Request) (*davClient, bool) {
	m, ok := ms.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "网盘不存在")
		return nil, false
	}
	c, err := newDavClient(m)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return c, true
}

func (ms *Mounts) handleBrowse(w http.ResponseWriter, r *http.Request) {
	c, ok := ms.client(w, r)
	if !ok {
		return
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	p := cleanDavPath(r.URL.Query().Get("path"))
	entries, err := c.list(ctx, p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": strings.TrimPrefix(p, "/"), "entries": entries})
}

// handleDownload streams a remote file through the NAS to the browser.
func (ms *Mounts) handleDownload(w http.ResponseWriter, r *http.Request) {
	c, ok := ms.client(w, r)
	if !ok {
		return
	}
	defer c.close()
	p := cleanDavPath(r.URL.Query().Get("path"))
	if p == "/" {
		writeErr(w, http.StatusBadRequest, "请指定要下载的文件")
		return
	}
	resp, err := c.get(r.Context(), p, r.Header.Get("Range"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连接失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		writeErr(w, http.StatusBadGateway, davStatusErr(resp).Error())
		return
	}
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", sanitizeName(path.Base(p))))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type saveJob struct {
	remote string // path on the mount
	dir    string // destination directory on the NAS
}

const maxSaveFiles = 2000

// collect walks a remote folder and lists every file under it.
func collect(ctx context.Context, c *davClient, remote, dest string, depth int, out *[]saveJob) error {
	if depth > 16 {
		return errors.New("文件夹层级太深")
	}
	entries, err := c.list(ctx, remote)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Dir {
			if err := collect(ctx, c, e.Path, path.Join(dest, sanitizeName(e.Name)), depth+1, out); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, saveJob{remote: e.Path, dir: dest})
		if len(*out) > maxSaveFiles {
			return fmt.Errorf("一次最多保存 %d 个文件，请分批操作", maxSaveFiles)
		}
	}
	return nil
}

// handleSave copies files (and folders, recursively) from the mount into the NAS as download tasks.
func (ms *Mounts) handleSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Items []struct {
			Path string `json:"path"`
			Dir  bool   `json:"dir"`
		} `json:"items"`
		Dir string `json:"dir"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	mount, ok := ms.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "网盘不存在")
		return
	}
	if strings.TrimSpace(in.Dir) == "" {
		in.Dir = path.Join(defaultDownloadDir, sanitizeName(mount.Name))
	}
	dest, err := cleanRel(in.Dir)
	if err != nil {
		fileErr(w, err)
		return
	}
	c, err := newDavClient(mount)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var jobs []saveJob
	for _, it := range in.Items {
		p := cleanDavPath(it.Path)
		if p == "/" {
			continue
		}
		if it.Dir {
			if err := collect(ctx, c, p, path.Join(dest, sanitizeName(path.Base(p))), 0, &jobs); err != nil {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
		} else {
			jobs = append(jobs, saveJob{remote: strings.TrimPrefix(p, "/"), dir: dest})
		}
	}
	if len(jobs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可保存的文件")
		return
	}
	for _, j := range jobs {
		d := Download{
			URL: mount.Name + ":/" + strings.TrimPrefix(j.remote, "/"), Name: sanitizeName(path.Base(j.remote)),
			Dir: j.dir, MountID: mount.ID, MountPath: j.remote,
		}
		if _, err := ms.dl.enqueue(d); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存任务失败: "+err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"added": len(jobs), "dir": dest})
}
