package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

const defaultDownloadDir = "离线下载"

// Downloads runs offline download tasks: the NAS fetches a URL (or a file on
// an external mount) into the local files directory, resuming where it can.
type Downloads struct {
	st     *Store
	files  *Files
	client *http.Client
	sem    chan struct{} // limits concurrent transfers

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	running map[string]chan struct{} // closed when the task's goroutine has fully stopped
}

func NewDownloads(st *Store, files *Files, concurrency int) *Downloads {
	tr := insecureTransport(false)
	tr.Proxy = http.ProxyFromEnvironment
	return &Downloads{
		st: st, files: files, client: &http.Client{Transport: tr},
		sem:     make(chan struct{}, concurrency),
		cancels: map[string]context.CancelFunc{},
		running: map[string]chan struct{}{},
	}
}

// Start resumes the tasks that were unfinished when the server last stopped.
func (m *Downloads) Start() {
	m.st.mu.Lock()
	var ids []string
	for _, d := range m.st.Downloads {
		if d.Status == "running" || d.Status == "queued" {
			d.Status = "queued"
			d.Speed = 0
			ids = append(ids, d.ID)
		}
	}
	m.st.mu.Unlock()
	for _, id := range ids {
		go m.run(id)
	}
}

// active counts the tasks that are running or waiting to run.
func (m *Downloads) active() int {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	n := 0
	for _, d := range m.st.Downloads {
		if d.Status == "running" || d.Status == "queued" {
			n++
		}
	}
	return n
}

// find returns the task with the given id. The caller holds st.mu.
func (m *Downloads) find(id string) *Download {
	for _, d := range m.st.Downloads {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// enqueue stores a new task and starts it.
func (m *Downloads) enqueue(d Download) (Download, error) {
	m.st.mu.Lock()
	d.ID = m.st.nextID("d")
	d.Status = "queued"
	d.Created = time.Now()
	m.st.Downloads = append(m.st.Downloads, &d)
	err := m.st.save()
	out := d
	m.st.mu.Unlock()
	if err != nil {
		return out, err
	}
	go m.run(out.ID)
	return out, nil
}

func partPath(d Download) string {
	return path.Join(d.Dir, "."+d.ID+partSuffix)
}

func (m *Downloads) run(id string) {
	// Never let two goroutines write the same task: wait for an earlier one to unwind.
	for {
		m.mu.Lock()
		ch := m.running[id]
		if ch == nil {
			m.running[id] = make(chan struct{})
			m.mu.Unlock()
			break
		}
		m.mu.Unlock()
		<-ch
	}
	defer func() {
		m.mu.Lock()
		close(m.running[id])
		delete(m.running, id)
		m.mu.Unlock()
	}()

	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.st.mu.Lock()
	d := m.find(id)
	if d == nil || d.Status != "queued" {
		m.st.mu.Unlock()
		return
	}
	d.Status, d.Error, d.Speed = "running", "", 0
	snap := *d
	_ = m.st.save()
	m.st.mu.Unlock()

	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()

	finalName, err := m.fetch(ctx, snap)

	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	d = m.find(id)
	if d == nil { // removed while running
		_ = m.files.root.Remove(osName(partPath(snap)))
		return
	}
	d.Speed = 0
	switch {
	case ctx.Err() != nil: // paused or canceled by the user; the status is already set
		if d.Status == "canceled" {
			_ = m.files.root.Remove(osName(partPath(snap)))
			d.Done = 0
		}
	case err != nil:
		d.Status, d.Error = "error", err.Error()
	default:
		d.Status, d.Name, d.Finished = "done", finalName, time.Now()
		if d.Total < d.Done {
			d.Total = d.Done
		}
	}
	_ = m.st.save()
}

// open starts the transfer at offset and returns the response plus a cleanup func.
func (m *Downloads) open(ctx context.Context, d Download, offset int64) (*http.Response, func(), error) {
	rng := ""
	if offset > 0 {
		rng = fmt.Sprintf("bytes=%d-", offset)
	}
	if d.MountID != "" {
		m.st.mu.Lock()
		var mount *Mount
		for _, mt := range m.st.Mounts {
			if mt.ID == d.MountID {
				c := *mt
				mount = &c
			}
		}
		m.st.mu.Unlock()
		if mount == nil {
			return nil, nil, errors.New("对应的外部网盘已被移除")
		}
		c, err := newDavClient(*mount)
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.get(ctx, d.MountPath, rng)
		if err != nil {
			c.close()
			return nil, nil, err
		}
		return resp, c.close, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, nil, errors.New("链接地址不正确")
	}
	req.Header.Set("User-Agent", "mynas/0.1")
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	return resp, func() {}, nil
}

func (m *Downloads) fetch(ctx context.Context, d Download) (string, error) {
	root := m.files.root
	if err := root.MkdirAll(osName(d.Dir), 0o755); err != nil {
		return "", fmt.Errorf("无法创建目录: %w", err)
	}
	part := partPath(d)
	var offset int64
	if info, err := root.Stat(osName(part)); err == nil {
		offset = info.Size()
	}

	resp, cleanup, err := m.open(ctx, d, offset)
	if err != nil {
		return "", connErr(ctx, err)
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0 {
		resp.Body.Close()
		cleanup()
		offset = 0
		if resp, cleanup, err = m.open(ctx, d, 0); err != nil {
			return "", connErr(ctx, err)
		}
	}
	defer cleanup()
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0 // the server ignored Range; start over
	case http.StatusPartialContent:
	default:
		return "", fmt.Errorf("服务器返回 %s", resp.Status)
	}
	total := int64(0)
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	name := d.Name
	if d.AutoName {
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
			name = sanitizeName(path.Base(strings.ReplaceAll(params["filename"], "\\", "/")))
		}
	}

	if err := m.files.spaceErr(total - offset); err != nil {
		return "", err
	}

	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if offset > 0 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	out, err := root.OpenFile(osName(part), flags, 0o644)
	if err != nil {
		return "", fmt.Errorf("无法写入文件: %w", err)
	}

	m.st.mu.Lock()
	if cur := m.find(d.ID); cur != nil {
		cur.Total, cur.Done, cur.Name = total, offset, name
	}
	m.st.mu.Unlock()

	done := offset
	buf := make([]byte, 128<<10)
	lastTick, tickBytes := time.Now(), int64(0)
	sinceCheck := int64(0)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return "", fmt.Errorf("写入失败: %w", werr)
			}
			done += int64(n)
			tickBytes += int64(n)
			// The size is not always known up front, so keep checking while writing.
			if sinceCheck += int64(n); sinceCheck >= 16<<20 {
				sinceCheck = 0
				if err := m.files.spaceErr(0); err != nil {
					out.Close()
					return "", err
				}
			}
			m.st.mu.Lock()
			if cur := m.find(d.ID); cur != nil {
				cur.Done = done
				if el := time.Since(lastTick); el >= time.Second {
					cur.Speed = int64(float64(tickBytes) / el.Seconds())
					lastTick, tickBytes = time.Now(), 0
				}
			}
			m.st.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return "", connErr(ctx, fmt.Errorf("下载中断: %w", rerr))
		}
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("写入失败: %w", err)
	}
	if total > 0 && done != total {
		return "", fmt.Errorf("文件不完整：收到 %d / %d 字节", done, total)
	}
	final := m.files.uniqueName(d.Dir, name)
	if err := root.Rename(osName(part), osName(path.Join(d.Dir, final))); err != nil {
		return "", fmt.Errorf("保存文件失败: %w", err)
	}
	return final, nil
}

func connErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// nameFromURL guesses a file name from the last path segment of a URL.
func nameFromURL(u *url.URL) string {
	base := path.Base(u.Path)
	if base == "" || base == "/" || base == "." {
		return "download"
	}
	return sanitizeName(base)
}

func (m *Downloads) handleList(w http.ResponseWriter, r *http.Request) {
	m.st.mu.Lock()
	out := make([]Download, 0, len(m.st.Downloads))
	for i := len(m.st.Downloads) - 1; i >= 0; i-- { // newest first
		out = append(out, *m.st.Downloads[i])
	}
	m.st.mu.Unlock()
	writeJSON(w, map[string]any{"downloads": out})
}

func (m *Downloads) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URLs []string `json:"urls"`
		Dir  string   `json:"dir"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Dir) == "" {
		in.Dir = defaultDownloadDir
	}
	dir, err := cleanRel(in.Dir)
	if err != nil {
		fileErr(w, err)
		return
	}
	var parsed []*url.URL
	for _, raw := range in.URLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			writeErr(w, http.StatusBadRequest, "目前只支持 http/https 直链，这个链接无法下载："+raw)
			return
		}
		parsed = append(parsed, u)
	}
	if len(parsed) == 0 {
		writeErr(w, http.StatusBadRequest, "请填写下载链接")
		return
	}
	if len(parsed) > 100 {
		writeErr(w, http.StatusBadRequest, "一次最多添加 100 个链接")
		return
	}
	for _, u := range parsed {
		if _, err := m.enqueue(Download{URL: u.String(), Name: nameFromURL(u), AutoName: true, Dir: dir}); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存任务失败: "+err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"added": len(parsed)})
}

func (m *Downloads) cancelRunning(id string) {
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Downloads) handleAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	m.st.mu.Lock()
	d := m.find(id)
	if d == nil {
		m.st.mu.Unlock()
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	start, stop := false, false
	switch action {
	case "pause":
		if d.Status == "running" || d.Status == "queued" {
			stop = d.Status == "running"
			d.Status, d.Speed = "paused", 0
		}
	case "cancel":
		switch d.Status {
		case "running":
			stop = true
			d.Status, d.Speed = "canceled", 0
		case "queued", "paused", "error":
			d.Status, d.Done = "canceled", 0
			_ = m.files.root.Remove(osName(partPath(*d)))
		}
	case "resume", "retry":
		if d.Status == "paused" || d.Status == "error" || d.Status == "canceled" {
			d.Status, d.Error = "queued", ""
			start = true
		}
	default:
		m.st.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "不支持的操作")
		return
	}
	_ = m.st.save()
	out := *d
	m.st.mu.Unlock()
	if stop {
		m.cancelRunning(id)
	}
	if start {
		go m.run(id)
	}
	writeJSON(w, out)
}

// handleDelete removes the task record. A finished file stays in the files directory.
func (m *Downloads) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.st.mu.Lock()
	var removed *Download
	for i, d := range m.st.Downloads {
		if d.ID == id {
			removed = d
			m.st.Downloads = append(m.st.Downloads[:i], m.st.Downloads[i+1:]...)
			break
		}
	}
	if removed == nil {
		m.st.mu.Unlock()
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	wasRunning := removed.Status == "running"
	if !wasRunning && removed.Status != "done" {
		_ = m.files.root.Remove(osName(partPath(*removed)))
	}
	_ = m.st.save()
	m.st.mu.Unlock()
	if wasRunning {
		m.cancelRunning(id) // the goroutine deletes the partial file when it stops
	}
	writeJSON(w, map[string]any{"ok": true})
}
