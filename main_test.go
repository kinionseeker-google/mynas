package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- unit tests ----------

func TestCleanRel(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"a/b":            "a/b",
		"/a//b/":         "a/b",
		"../../etc":      "etc",
		"a/../../b":      "b",
		`a\..\..\secret`: "secret",
	}
	for in, want := range cases {
		got, err := cleanRel(in)
		if err != nil || got != want {
			t.Errorf("cleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{".trash", ".trash/t1", "a/.trash/x", "x/.d1" + partSuffix} {
		if _, err := cleanRel(in); err == nil {
			t.Errorf("cleanRel(%q) should be rejected", in)
		}
	}
}

func TestNames(t *testing.T) {
	for _, n := range []string{"a.txt", "照片 2026", ".hidden"} {
		if !validName(n) {
			t.Errorf("validName(%q) = false", n)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, "a:b", "x.", "x ", ".trash", "f" + partSuffix, "a\x00b"} {
		if validName(n) {
			t.Errorf("validName(%q) = true", n)
		}
	}
	if got := sanitizeName(`a/b:c?.zip`); got != "a_b_c_.zip" {
		t.Errorf("sanitizeName = %q", got)
	}
	if got := sanitizeName("..."); got != "download" {
		t.Errorf("sanitizeName(...) = %q", got)
	}
}

func TestFraming(t *testing.T) {
	got := stripFrameAncestors("default-src 'self'; frame-ancestors 'none'; img-src *")
	if strings.Contains(got, "frame-ancestors") || !strings.Contains(got, "default-src 'self'") || !strings.Contains(got, "img-src *") {
		t.Errorf("stripFrameAncestors = %q", got)
	}
	h := http.Header{}
	h.Set("X-Frame-Options", "DENY")
	h.Add("Content-Security-Policy", "frame-ancestors 'self'")
	if blocked, _ := framingBlocked(h); !blocked {
		t.Error("framingBlocked should be true")
	}
	allowFraming(h)
	if blocked, _ := framingBlocked(h); blocked || h.Get("Content-Security-Policy") != "" {
		t.Errorf("allowFraming left %v", h)
	}
}

// ---------- end-to-end tests against a real handler ----------

type client struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
}

func (c *client) req(method, path string, body any, header bool) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if header {
		req.Header.Set("X-Requested-With", "mynas")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no redirect following
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// call does an authenticated JSON call and decodes the reply.
func (c *client) call(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	resp, data := c.req(method, path, body, true)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return out
}

func freePortRange(t *testing.T) (int, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p, p + 5
}

func newTestApp(t *testing.T) (*App, *client, string) {
	return newTestAppWith(t, Options{})
}

func newTestAppWith(t *testing.T, o Options) (*App, *client, string) {
	dir := t.TempDir()
	o.DataDir, o.BindHost = dir, "127.0.0.1"
	o.PortMin, o.PortMax = freePortRange(t)
	app, err := NewApp(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		srv.Close()
		app.st.mu.Lock()
		ids := []string{}
		for _, l := range app.st.Links {
			ids = append(ids, l.ID)
		}
		app.st.mu.Unlock()
		for _, id := range ids {
			app.links.stopProxy(id)
		}
		app.files.root.Close()
	})
	c := &client{t: t, base: srv.URL}
	resp, data := c.req("POST", "/api/setup", map[string]string{"username": "admin", "password": "correct horse"}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d %s", resp.StatusCode, data)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName {
			c.cookie = ck
		}
	}
	if c.cookie == nil {
		t.Fatal("no session cookie after setup")
	}
	return app, c, dir
}

func TestAuthAndCSRF(t *testing.T) {
	_, c, dir := newTestApp(t)

	anon := &client{t: t, base: c.base}
	if resp, _ := anon.req("GET", "/api/files/list", nil, true); resp.StatusCode != 401 {
		t.Errorf("anonymous list = %d, want 401", resp.StatusCode)
	}
	if resp, _ := c.req("POST", "/api/files/mkdir", map[string]string{"name": "x"}, false); resp.StatusCode != 403 {
		t.Errorf("write without header = %d, want 403", resp.StatusCode)
	}
	if resp, _ := anon.req("POST", "/api/setup", map[string]string{"username": "evil", "password": "12345678"}, true); resp.StatusCode != 409 {
		t.Errorf("second setup = %d, want 409", resp.StatusCode)
	}
	if resp, _ := anon.req("POST", "/api/login", map[string]string{"username": "admin", "password": "wrong"}, true); resp.StatusCode != 401 {
		t.Errorf("bad login = %d, want 401", resp.StatusCode)
	}
	if resp, _ := anon.req("POST", "/api/login", map[string]string{"username": "admin", "password": "correct horse"}, true); resp.StatusCode != 200 {
		t.Errorf("good login = %d, want 200", resp.StatusCode)
	}
	// The password must not be stored in clear text.
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(state, []byte("correct horse")) {
		t.Error("state.json contains the plain password")
	}
	// Lockout after repeated failures.
	for i := 0; i < maxFails; i++ {
		anon.req("POST", "/api/login", map[string]string{"username": "admin", "password": "nope"}, true)
	}
	if resp, _ := anon.req("POST", "/api/login", map[string]string{"username": "admin", "password": "correct horse"}, true); resp.StatusCode != 429 {
		t.Errorf("login during lockout = %d, want 429", resp.StatusCode)
	}
}

func TestFilesFlow(t *testing.T) {
	_, c, dir := newTestApp(t)
	// A file next to the files root must stay unreachable.
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	c.call("POST", "/api/files/mkdir", map[string]string{"dir": "", "name": "照片"}, 200)
	content := []byte("hello nas")
	resp, data := c.req("POST", "/api/files/upload?dir="+url.QueryEscape("照片")+"&name="+url.QueryEscape("a b.txt"), content, true)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, data)
	}
	// Same name again gets a numbered copy instead of overwriting.
	c.req("POST", "/api/files/upload?dir="+url.QueryEscape("照片")+"&name="+url.QueryEscape("a b.txt"), []byte("second"), true)

	list := c.call("GET", "/api/files/list?path="+url.QueryEscape("照片"), nil, 200)
	if n := len(list["entries"].([]any)); n != 2 {
		t.Fatalf("entries = %d, want 2: %v", n, list)
	}
	resp, data = c.req("GET", "/api/files/download?path="+url.QueryEscape("照片/a b.txt"), nil, false)
	if resp.StatusCode != 200 || !bytes.Equal(data, content) {
		t.Fatalf("download = %d %q", resp.StatusCode, data)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	for _, p := range []string{"../secret.txt", "..%2Fsecret.txt", `..\secret.txt`, "照片/../../secret.txt", "../state.json"} {
		if resp, data := c.req("GET", "/api/files/download?path="+p, nil, false); resp.StatusCode == 200 {
			t.Errorf("traversal %q returned 200: %q", p, data)
		}
	}

	c.call("POST", "/api/files/rename", map[string]string{"path": "照片/a b.txt", "name": "renamed.txt"}, 200)
	found := c.call("GET", "/api/files/find?q=RENAMED", nil, 200)
	if n := len(found["entries"].([]any)); n != 1 {
		t.Fatalf("find = %v", found)
	}

	c.call("POST", "/api/files/delete", map[string]any{"paths": []string{"照片/renamed.txt"}}, 200)
	if resp, _ := c.req("GET", "/api/files/download?path="+url.QueryEscape("照片/renamed.txt"), nil, false); resp.StatusCode != 404 {
		t.Errorf("deleted file download = %d, want 404", resp.StatusCode)
	}
	if resp, _ := c.req("GET", "/api/files/list?path=.trash", nil, false); resp.StatusCode == 200 {
		t.Error(".trash must not be listable")
	}
	trash := c.call("GET", "/api/trash", nil, 200)
	items := trash["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("trash = %v", trash)
	}
	id := items[0].(map[string]any)["id"].(string)
	c.call("POST", "/api/trash/restore", map[string]any{"ids": []string{id}}, 200)
	resp, data = c.req("GET", "/api/files/download?path="+url.QueryEscape("照片/renamed.txt"), nil, false)
	if resp.StatusCode != 200 || !bytes.Equal(data, content) {
		t.Fatalf("restored download = %d %q", resp.StatusCode, data)
	}
	st := c.call("GET", "/api/storage", nil, 200)
	if st["total"].(float64) <= 0 || st["dir"] != filepath.Join(dir, "files") {
		t.Errorf("storage = %v", st)
	}
}

func TestLinkProxy(t *testing.T) {
	_, c, _ := newTestApp(t)

	var mu sync.Mutex
	var gotCookie, gotHost string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotCookie, gotHost = r.Header.Get("Cookie"), r.Host
		mu.Unlock()
		if r.URL.Path == "/go" {
			http.Redirect(w, r, "http://"+r.Host+"/landed?x=1", http.StatusFound)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "1", Secure: true, Domain: "example.com"})
		fmt.Fprint(w, "target page "+r.URL.Path)
	}))
	defer target.Close()

	if resp, _ := c.req("POST", "/api/links", map[string]any{"name": "x", "url": "ftp://a/b", "mode": "new"}, true); resp.StatusCode != 400 {
		t.Errorf("ftp link = %d, want 400", resp.StatusCode)
	}
	link := c.call("POST", "/api/links", map[string]any{"name": "路由器", "url": target.URL + "/admin", "mode": "embed", "proxy": true, "pin": true}, 200)
	port := int(link["proxyPort"].(float64))
	if port == 0 {
		t.Fatalf("no proxy port: %v", link)
	}
	id := link["id"].(string)

	check := c.call("GET", "/api/links/"+id+"/check", nil, 200)
	if check["reachable"] != true || check["frameBlocked"] != true {
		t.Errorf("check = %v", check)
	}

	proxy := &client{t: t, base: "http://127.0.0.1:" + strconv.Itoa(port)}
	if resp, _ := proxy.req("GET", "/admin", nil, false); resp.StatusCode != 401 {
		t.Fatalf("proxy without session = %d, want 401", resp.StatusCode)
	}
	proxy.cookie = c.cookie
	resp, data := proxy.req("GET", "/admin", nil, false)
	if resp.StatusCode != 200 || string(data) != "target page /admin" {
		t.Fatalf("proxy = %d %q", resp.StatusCode, data)
	}
	if resp.Header.Get("X-Frame-Options") != "" || strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors") {
		t.Errorf("framing headers not stripped: %v", resp.Header)
	}
	if sc := resp.Header.Get("Set-Cookie"); strings.Contains(strings.ToLower(sc), "secure") || strings.Contains(strings.ToLower(sc), "domain") {
		t.Errorf("Set-Cookie not rewritten: %q", sc)
	}
	mu.Lock()
	if strings.Contains(gotCookie, cookieName) {
		t.Errorf("NAS session cookie leaked to the target: %q", gotCookie)
	}
	if want := strings.TrimPrefix(target.URL, "http://"); gotHost != want {
		t.Errorf("target saw Host %q, want %q", gotHost, want)
	}
	mu.Unlock()

	resp, _ = proxy.req("GET", "/go", nil, false)
	if loc := resp.Header.Get("Location"); loc != "/landed?x=1" {
		t.Errorf("Location = %q, want a relative path", loc)
	}

	// Turning the proxy off closes the port.
	c.call("PUT", "/api/links/"+id, map[string]any{"name": "路由器", "url": target.URL + "/admin", "mode": "embed", "proxy": false}, 200)
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		conn.Close()
		t.Error("proxy port still open after disabling")
	}
	c.call("DELETE", "/api/links/"+id, nil, 200)
}

func waitDownload(t *testing.T, c *client, id string, want func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		list := c.call("GET", "/api/downloads", nil, 200)["downloads"].([]any)
		for _, it := range list {
			d := it.(map[string]any)
			if id == "" || d["id"] == id {
				last = d
				if want(d) {
					return d
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("download did not reach the expected state; last = %v", last)
	return nil
}

func TestOfflineDownloadResume(t *testing.T) {
	_, c, dir := newTestApp(t)
	payload := make([]byte, 600<<10)
	rand.Read(payload)

	var mu sync.Mutex
	sawRange := false
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if rng := r.Header.Get("Range"); rng != "" {
			fmt.Sscanf(rng, "bytes=%d-", &start)
			mu.Lock()
			sawRange = true
			mu.Unlock()
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Disposition", `attachment; filename="real name.bin"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		}
		for i := start; i < len(payload); i += 32 << 10 {
			end := min(i+32<<10, len(payload))
			if _, err := w.Write(payload[i:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(15 * time.Millisecond)
		}
	}))
	defer src.Close()

	if resp, _ := c.req("POST", "/api/downloads", map[string]any{"urls": []string{"magnet:?xt=urn:btih:abc"}}, true); resp.StatusCode != 400 {
		t.Errorf("magnet link = %d, want 400", resp.StatusCode)
	}
	c.call("POST", "/api/downloads", map[string]any{"urls": []string{src.URL + "/files/x.bin"}, "dir": "下载/测试"}, 200)
	d := waitDownload(t, c, "", func(d map[string]any) bool { return d["status"] == "running" && d["done"].(float64) > 0 })
	id := d["id"].(string)

	c.call("POST", "/api/downloads/"+id+"/pause", nil, 200)
	time.Sleep(200 * time.Millisecond)
	paused := waitDownload(t, c, id, func(d map[string]any) bool { return d["status"] == "paused" })
	if done := paused["done"].(float64); done <= 0 || done >= float64(len(payload)) {
		t.Fatalf("paused at %v bytes of %d", done, len(payload))
	}

	c.call("POST", "/api/downloads/"+id+"/resume", nil, 200)
	done := waitDownload(t, c, id, func(d map[string]any) bool { return d["status"] == "done" || d["status"] == "error" })
	if done["status"] != "done" {
		t.Fatalf("download failed: %v", done)
	}
	if done["name"] != "real name.bin" {
		t.Errorf("name = %v, want the Content-Disposition name", done["name"])
	}
	got, err := os.ReadFile(filepath.Join(dir, "files", "下载", "测试", "real name.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("downloaded file differs (err=%v, %d bytes)", err, len(got))
	}
	mu.Lock()
	if !sawRange {
		t.Error("resume did not use a Range request")
	}
	mu.Unlock()
	// No partial file is left behind, and it never shows up in listings.
	list := c.call("GET", "/api/files/list?path="+url.QueryEscape("下载/测试"), nil, 200)
	if n := len(list["entries"].([]any)); n != 1 {
		t.Errorf("entries = %v", list)
	}
}

func TestDownloadError(t *testing.T) {
	_, c, _ := newTestApp(t)
	src := httptest.NewServer(http.NotFoundHandler())
	defer src.Close()
	c.call("POST", "/api/downloads", map[string]any{"urls": []string{src.URL + "/missing.zip"}}, 200)
	d := waitDownload(t, c, "", func(d map[string]any) bool { return d["status"] == "error" })
	if !strings.Contains(d["error"].(string), "404") {
		t.Errorf("error = %v", d["error"])
	}
}

// fakeDAV is a tiny read-only WebDAV server rooted at /dav/.
func fakeDAV(t *testing.T, user, pass string, files map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rel := strings.TrimPrefix(r.URL.Path, "/dav")
		switch r.Method {
		case "PROPFIND":
			dir := strings.TrimSuffix(rel, "/") + "/"
			var b strings.Builder
			b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
			item := func(p string, isDir bool, size int) {
				href := (&url.URL{Path: "/dav" + p}).EscapedPath()
				b.WriteString("<d:response><d:href>" + href + "</d:href><d:propstat><d:prop>")
				if isDir {
					b.WriteString("<d:resourcetype><d:collection/></d:resourcetype>")
				} else {
					fmt.Fprintf(&b, "<d:resourcetype/><d:getcontentlength>%d</d:getcontentlength>", size)
				}
				b.WriteString("<d:getlastmodified>Mon, 02 Nov 2026 08:00:00 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>")
			}
			item(dir, true, 0)
			seen := map[string]bool{}
			for p, body := range files {
				if !strings.HasPrefix(p, dir) {
					continue
				}
				rest := strings.TrimPrefix(p, dir)
				if name, _, sub := strings.Cut(rest, "/"); sub {
					if !seen[name] {
						seen[name] = true
						item(dir+name+"/", true, 0)
					}
				} else {
					item(p, false, len(body))
				}
			}
			b.WriteString("</d:multistatus>")
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, b.String())
		case "GET":
			body, ok := files[rel]
			if !ok {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func TestWebDAVMount(t *testing.T) {
	_, c, dir := newTestApp(t)
	dav := fakeDAV(t, "me", "app-pass", map[string]string{
		"/readme.txt":        "root file",
		"/照片/海 边.jpg":        "jpeg bytes",
		"/照片/2026/trip.mp4":  "video bytes",
		"/照片/2026/notes.txt": "notes",
	})
	defer dav.Close()

	bad := map[string]any{"name": "盘", "url": dav.URL + "/dav/", "username": "me", "password": "wrong"}
	if resp, data := c.req("POST", "/api/mounts/test", bad, true); resp.StatusCode == 200 {
		t.Errorf("test with a wrong password succeeded: %s", data)
	}
	if resp, _ := c.req("POST", "/api/mounts/test", map[string]any{"name": "盘", "url": c.base + "/"}, true); resp.StatusCode == 200 {
		t.Error("a non-WebDAV address passed the test")
	}

	m := c.call("POST", "/api/mounts", map[string]any{"name": "我的网盘", "url": dav.URL + "/dav/", "username": "me", "password": "app-pass"}, 200)
	id := m["id"].(string)
	if _, leaked := m["password"]; leaked {
		t.Error("mount reply contains the password")
	}
	_, raw := c.req("GET", "/api/mounts", nil, false)
	if bytes.Contains(raw, []byte("app-pass")) {
		t.Error("mount list leaks the password")
	}

	root := c.call("GET", "/api/mounts/"+id+"/list?path=", nil, 200)["entries"].([]any)
	if len(root) != 2 {
		t.Fatalf("root entries = %v", root)
	}
	sub := c.call("GET", "/api/mounts/"+id+"/list?path="+url.QueryEscape("照片"), nil, 200)["entries"].([]any)
	names := map[string]bool{}
	for _, e := range sub {
		names[e.(map[string]any)["path"].(string)] = e.(map[string]any)["dir"].(bool)
	}
	if isDir, ok := names["照片/2026"]; !ok || !isDir {
		t.Errorf("sub entries = %v", names)
	}
	if isDir, ok := names["照片/海 边.jpg"]; !ok || isDir {
		t.Errorf("sub entries = %v", names)
	}

	resp, data := c.req("GET", "/api/mounts/"+id+"/download?path="+url.QueryEscape("照片/海 边.jpg"), nil, false)
	if resp.StatusCode != 200 || string(data) != "jpeg bytes" {
		t.Fatalf("mount download = %d %q", resp.StatusCode, data)
	}

	// Save a folder (recursively) and a single file into the NAS.
	saved := c.call("POST", "/api/mounts/"+id+"/save", map[string]any{
		"items": []map[string]any{{"path": "照片", "dir": true}, {"path": "readme.txt", "dir": false}},
		"dir":   "来自网盘",
	}, 200)
	if saved["added"].(float64) != 4 {
		t.Fatalf("save = %v", saved)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		list := c.call("GET", "/api/downloads", nil, 200)["downloads"].([]any)
		pending := 0
		for _, it := range list {
			d := it.(map[string]any)
			if d["status"] == "error" {
				t.Fatalf("task failed: %v", d)
			}
			if d["status"] != "done" {
				pending++
			}
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("save tasks did not finish")
		}
		time.Sleep(30 * time.Millisecond)
	}
	for rel, want := range map[string]string{
		"来自网盘/readme.txt":        "root file",
		"来自网盘/照片/海 边.jpg":        "jpeg bytes",
		"来自网盘/照片/2026/trip.mp4":  "video bytes",
		"来自网盘/照片/2026/notes.txt": "notes",
	} {
		got, err := os.ReadFile(filepath.Join(dir, "files", filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", rel, got, err)
		}
	}
	c.call("DELETE", "/api/mounts/"+id, nil, 200)
}

// A separate files directory keeps state.json out of it, and the free-space
// floor refuses writes instead of filling the disk.
func TestFilesDirAndMinFree(t *testing.T) {
	filesDir := t.TempDir()
	_, c, dataDir := newTestAppWith(t, Options{FilesDir: filesDir})
	resp, data := c.req("POST", "/api/files/upload?dir=&name=a.txt", []byte("hi"), true)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(filesDir, "a.txt")); err != nil {
		t.Errorf("file not in the files directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "state.json")); err != nil {
		t.Errorf("state.json not in the data directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "files")); err == nil {
		t.Error("default files directory was created although a files directory is set")
	}

	_, full, fullDir := newTestAppWith(t, Options{MinFree: 1 << 60})
	resp, data = full.req("POST", "/api/files/upload?dir=&name=a.txt", []byte("hi"), true)
	if resp.StatusCode != http.StatusInsufficientStorage || !strings.Contains(string(data), "空间不足") {
		t.Errorf("upload on a full disk = %d %s", resp.StatusCode, data)
	}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "payload") }))
	defer src.Close()
	full.call("POST", "/api/downloads", map[string]any{"urls": []string{src.URL + "/f.bin"}}, 200)
	d := waitDownload(t, full, "", func(d map[string]any) bool { return d["status"] == "error" })
	if !strings.Contains(d["error"].(string), "空间不足") {
		t.Errorf("download error = %v", d["error"])
	}
	left, _ := os.ReadDir(filepath.Join(fullDir, "files", defaultDownloadDir))
	for _, e := range left {
		if !strings.HasSuffix(e.Name(), partSuffix) {
			t.Errorf("unexpected file written on a full disk: %s", e.Name())
		}
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		age  time.Duration
		days int
		want bool
	}{
		{29 * day, 30, false},
		{30 * day, 30, true},
		{400 * day, 0, false}, // 0 keeps everything
		{-5 * day, 7, false},  // "deleted in the future": the clock went backwards
		{8 * day, 7, true},
	}
	for _, c := range cases {
		if got := expired(now.Add(-c.age), now, c.days); got != c.want {
			t.Errorf("expired(age %v, %d days) = %v, want %v", c.age, c.days, got, c.want)
		}
	}
}

func TestTrashRetention(t *testing.T) {
	app, c, dir := newTestApp(t)
	trashFile := func(id string) string { return filepath.Join(dir, "files", trashDir, id) }
	// del uploads a file, deletes it and returns its trash id.
	del := func(name string) string {
		t.Helper()
		c.req("POST", "/api/files/upload?dir=&name="+name, []byte(name), true)
		c.call("POST", "/api/files/delete", map[string]any{"paths": []string{name}}, 200)
		for _, it := range c.call("GET", "/api/trash", nil, 200)["items"].([]any) {
			if m := it.(map[string]any); m["name"] == name {
				return m["id"].(string)
			}
		}
		t.Fatalf("%s not in the trash", name)
		return ""
	}
	// age makes an item look as if it was deleted that long ago. With forget it
	// also looks like an earlier run of the server deleted it.
	age := func(id string, d time.Duration, forget bool) {
		app.st.mu.Lock()
		defer app.st.mu.Unlock()
		for _, it := range app.st.Trash {
			if it.ID == id {
				it.DeletedAt = time.Now().Add(-d).Round(0)
			}
		}
		if forget {
			delete(app.files.trashSeen, id)
		}
	}
	exists := func(id string) bool { _, err := os.Stat(trashFile(id)); return err == nil }
	day := 24 * time.Hour

	if got := c.call("GET", "/api/trash", nil, 200)["trashDays"]; got != float64(defaultTrashDays) {
		t.Fatalf("default retention = %v, want %d", got, defaultTrashDays)
	}

	old, fresh, jumped := del("old.txt"), del("fresh.txt"), del("jumped.txt")
	age(old, 40*day, true)
	// Deleted a moment ago by this process, but the stored time is 40 days back:
	// what a late NTP correction leaves behind. It must not be purged.
	age(jumped, 40*day, false)

	if n := app.files.purgeExpired(time.Now()); n != 1 {
		t.Fatalf("purged %d items, want 1", n)
	}
	if exists(old) {
		t.Error("the 40-day-old item is still on disk")
	}
	if !exists(fresh) || !exists(jumped) {
		t.Error("an item that is not really expired was deleted")
	}
	items := c.call("GET", "/api/trash", nil, 200)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("trash has %d items, want 2", len(items))
	}
	for _, it := range items {
		m := it.(map[string]any)
		at, _ := time.Parse(time.RFC3339Nano, m["deletedAt"].(string))
		if time.Since(at) > time.Hour {
			t.Errorf("%s keeps a stale deletion time %v", m["name"], at)
		}
	}

	for _, bad := range []any{map[string]any{"trashDays": -1}, map[string]any{"trashDays": maxTrashDays + 1}, map[string]any{}} {
		if resp, _ := c.req("PUT", "/api/settings", bad, true); resp.StatusCode != 400 {
			t.Errorf("settings %v = %d, want 400", bad, resp.StatusCode)
		}
	}

	// 0 keeps everything, however old.
	c.call("PUT", "/api/settings", map[string]any{"trashDays": 0}, 200)
	age(fresh, 400*day, true)
	if n := app.files.purgeExpired(time.Now()); n != 0 || !exists(fresh) {
		t.Errorf("with retention off, purged %d items", n)
	}

	// Shortening the retention applies at once and is reported.
	age(fresh, 10*day, true)
	res := c.call("PUT", "/api/settings", map[string]any{"trashDays": 7}, 200)
	if res["purged"] != float64(1) || exists(fresh) || !exists(jumped) {
		t.Errorf("after shortening to 7 days: %v, fresh exists=%v, jumped exists=%v", res, exists(fresh), exists(jumped))
	}
	if got := c.call("GET", "/api/settings", nil, 200)["trashDays"]; got != float64(7) {
		t.Errorf("retention = %v, want 7", got)
	}
	// The setting is written to disk.
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !bytes.Contains(state, []byte(`"trashDays": 7`)) {
		t.Error("retention not saved in state.json")
	}
}

// The browser only shows a preview when the file comes back with the right
// type and "inline". Routers have no system MIME table, so check the types here.
func TestInlineTypes(t *testing.T) {
	_, c, _ := newTestApp(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	mp4 := append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), make([]byte, 64)...)
	cases := []struct {
		name string
		body []byte
		typ  string
		disp string
	}{
		{"a.png", png, "image/png", "inline"},
		{"b.jpg", jpg, "image/jpeg", "inline"},
		{"c.JPG", jpg, "image/jpeg", "inline"},
		{"d.jpeg", jpg, "image/jpeg", "inline"},
		{"e.mp4", mp4, "video/mp4", "inline"},
		{"f.svg", []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"), "image/svg+xml", "attachment"},
		{"g.txt", []byte("plain"), "text/plain", "attachment"},
	}
	for _, tc := range cases {
		if resp, data := c.req("POST", "/api/files/upload?dir=&name="+tc.name, tc.body, true); resp.StatusCode != 200 {
			t.Fatalf("upload %s: %d %s", tc.name, resp.StatusCode, data)
		}
		resp, _ := c.req("GET", "/api/files/download?inline=1&path="+tc.name, nil, false)
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, tc.typ) {
			t.Errorf("%s: Content-Type = %q, want %s", tc.name, got, tc.typ)
		}
		if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, tc.disp) {
			t.Errorf("%s: Content-Disposition = %q, want %s", tc.name, got, tc.disp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", tc.name)
		}
	}
}

// dav sends one WebDAV request with Basic credentials.
func dav(t *testing.T, base, method, p, user, pass string, body []byte, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+p, rd)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestWebDAV(t *testing.T) {
	_, c, dir := newTestApp(t)
	files := filepath.Join(dir, "files")
	onDisk := func(rel string) string { return filepath.Join(files, filepath.FromSlash(rel)) }

	// A file outside the shared folder that the key must never reach.
	c.req("POST", "/api/files/upload?dir=&name=secret.txt", []byte("top secret"), true)

	for _, bad := range []string{"", "a b", "-x", "名字", strings.Repeat("a", 33)} {
		if resp, _ := c.req("POST", "/api/dav", map[string]any{"username": bad, "dir": "x"}, true); resp.StatusCode != 400 {
			t.Errorf("username %q = %d, want 400", bad, resp.StatusCode)
		}
	}
	created := c.call("POST", "/api/dav", map[string]any{"username": "obsidian", "dir": "笔记/Obsidian"}, 200)
	pass, _ := created["password"].(string)
	if len(pass) < 30 {
		t.Fatalf("no usable password in %v", created)
	}
	id := created["key"].(map[string]any)["id"].(string)
	if resp, _ := c.req("POST", "/api/dav", map[string]any{"username": "obsidian", "dir": "x"}, true); resp.StatusCode != 409 {
		t.Errorf("duplicate username = %d, want 409", resp.StatusCode)
	}
	// The password is shown once and never stored or listed.
	_, list := c.req("GET", "/api/dav", nil, false)
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(list, []byte(pass)) || bytes.Contains(list, []byte("hash")) || bytes.Contains(state, []byte(pass)) {
		t.Error("the WebDAV password leaks through the key list or state.json")
	}

	u := "obsidian"
	if resp, _ := dav(t, c.base, "PROPFIND", "/dav/", "", "", nil, nil); resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("no credentials = %d %q, want 401 with a Basic challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp, _ := dav(t, c.base, "PROPFIND", "/dav/", u, "wrong", nil, nil); resp.StatusCode != 401 {
		t.Errorf("wrong password = %d, want 401", resp.StatusCode)
	}
	if resp, _ := dav(t, c.base, "PROPFIND", "/dav/", "admin", "correct horse", nil, nil); resp.StatusCode != 401 {
		t.Errorf("admin account over WebDAV = %d, want 401", resp.StatusCode)
	}
	if resp, _ := c.req("PROPFIND", "/dav/", nil, true); resp.StatusCode != 401 {
		t.Errorf("web session over WebDAV = %d, want 401", resp.StatusCode)
	}

	if resp, _ := dav(t, c.base, "OPTIONS", "/dav/", u, pass, nil, nil); resp.StatusCode != 200 || resp.Header.Get("DAV") == "" {
		t.Errorf("OPTIONS = %d, DAV header %q", resp.StatusCode, resp.Header.Get("DAV"))
	}
	for _, col := range []string{"/dav/vault", "/dav/vault/日记"} {
		if resp, body := dav(t, c.base, "MKCOL", col, u, pass, nil, nil); resp.StatusCode != 201 {
			t.Fatalf("MKCOL %s = %d %s", col, resp.StatusCode, body)
		}
	}
	note := "/dav/vault/" + url.PathEscape("日记") + "/note.md"
	if resp, body := dav(t, c.base, "PUT", note, u, pass, []byte("# 第一版"), nil); resp.StatusCode != 201 {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	if got, err := os.ReadFile(onDisk("笔记/Obsidian/vault/日记/note.md")); err != nil || string(got) != "# 第一版" {
		t.Fatalf("note on disk = %q, %v", got, err)
	}
	dav(t, c.base, "PUT", note, u, pass, []byte("# 第二版"), nil)
	if resp, body := dav(t, c.base, "GET", note, u, pass, nil, nil); resp.StatusCode != 200 || body != "# 第二版" {
		t.Errorf("GET = %d %q", resp.StatusCode, body)
	}
	resp, body := dav(t, c.base, "PROPFIND", "/dav/", u, pass, nil, map[string]string{"Depth": "infinity"})
	if resp.StatusCode != 207 || !strings.Contains(body, "note.md") || strings.Contains(body, "secret.txt") {
		t.Errorf("PROPFIND infinity = %d, body lists note.md: %v, lists secret.txt: %v", resp.StatusCode, strings.Contains(body, "note.md"), strings.Contains(body, "secret.txt"))
	}

	// MOVE (rename), then DELETE: the deleted note goes to the recycle bin.
	renamed := "/dav/vault/" + url.PathEscape("日记") + "/renamed.md"
	if resp, body := dav(t, c.base, "MOVE", note, u, pass, nil, map[string]string{"Destination": c.base + renamed}); resp.StatusCode != 201 {
		t.Fatalf("MOVE = %d %s", resp.StatusCode, body)
	}
	if resp, body := dav(t, c.base, "DELETE", renamed, u, pass, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("DELETE = %d %s", resp.StatusCode, body)
	}
	trash := c.call("GET", "/api/trash", nil, 200)["items"].([]any)
	if len(trash) != 1 || trash[0].(map[string]any)["origPath"] != "笔记/Obsidian/vault/日记/renamed.md" {
		t.Fatalf("trash after a WebDAV delete = %v", trash)
	}
	c.call("POST", "/api/trash/restore", map[string]any{"ids": []string{trash[0].(map[string]any)["id"].(string)}}, 200)
	if got, _ := os.ReadFile(onDisk("笔记/Obsidian/vault/日记/renamed.md")); string(got) != "# 第二版" {
		t.Errorf("restored note = %q", got)
	}

	// The key is confined to its folder.
	for _, p := range []string{"/dav/..%2F..%2Fsecret.txt", "/dav/%2e%2e/%2e%2e/secret.txt", "/dav/vault/..%2F..%2F..%2Fsecret.txt"} {
		if resp, body := dav(t, c.base, "GET", p, u, pass, nil, nil); strings.Contains(body, "top secret") {
			t.Errorf("GET %s escaped the folder (%d)", p, resp.StatusCode)
		}
	}
	dav(t, c.base, "MOVE", renamed, u, pass, nil, map[string]string{"Destination": c.base + "/dav/..%2F..%2Fescaped.md"})
	dav(t, c.base, "PUT", "/dav/..%2F..%2Fescaped2.md", u, pass, []byte("x"), nil)
	for _, rel := range []string{"escaped.md", "escaped2.md", "笔记/escaped.md", "笔记/escaped2.md"} {
		if _, err := os.Stat(onDisk(rel)); err == nil {
			t.Errorf("%s was written outside the key's folder", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.md")); err == nil {
		t.Error("a file was written outside the files directory")
	}

	// A key for everything still cannot see or touch the recycle bin.
	all := c.call("POST", "/api/dav", map[string]any{"username": "all", "dir": ""}, 200)["password"].(string)
	c.call("POST", "/api/files/delete", map[string]any{"paths": []string{"secret.txt"}}, 200)
	resp, body = dav(t, c.base, "PROPFIND", "/dav/", "all", all, nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != 207 || strings.Contains(body, trashDir) || !strings.Contains(body, url.PathEscape("笔记")) {
		t.Errorf("root PROPFIND = %d, shows the recycle bin: %v", resp.StatusCode, strings.Contains(body, trashDir))
	}
	if resp, _ := dav(t, c.base, "PROPFIND", "/dav/"+trashDir+"/", "all", all, nil, nil); resp.StatusCode == 207 {
		t.Error("the recycle bin can be listed over WebDAV")
	}
	dav(t, c.base, "PUT", "/dav/"+trashDir+"/planted", "all", all, []byte("x"), nil)
	if _, err := os.Stat(onDisk(trashDir + "/planted")); err == nil {
		t.Error("a file was written into the recycle bin over WebDAV")
	}
	if resp, _ := dav(t, c.base, "DELETE", "/dav/", "all", all, nil, nil); resp.StatusCode < 400 {
		t.Errorf("DELETE of the shared folder itself = %d, want a refusal", resp.StatusCode)
	}

	// Read-only keys can read but not change anything.
	ro := c.call("POST", "/api/dav", map[string]any{"username": "reader", "dir": "笔记/Obsidian", "readOnly": true}, 200)["password"].(string)
	dav(t, c.base, "PUT", note, u, pass, []byte("# 第二版"), nil) // the escape attempts above moved the earlier copy
	if resp, body := dav(t, c.base, "GET", note, "reader", ro, nil, nil); resp.StatusCode != 200 || body != "# 第二版" {
		t.Errorf("read-only GET = %d %q", resp.StatusCode, body)
	}
	for _, m := range []string{"PUT", "DELETE", "MKCOL"} {
		if resp, _ := dav(t, c.base, m, "/dav/vault/x", "reader", ro, []byte("x"), nil); resp.StatusCode != 403 {
			t.Errorf("read-only %s = %d, want 403", m, resp.StatusCode)
		}
	}

	// Removing a key locks its client out at once.
	c.call("DELETE", "/api/dav/"+id, nil, 200)
	if resp, _ := dav(t, c.base, "PROPFIND", "/dav/", u, pass, nil, nil); resp.StatusCode != 401 {
		t.Errorf("removed key = %d, want 401", resp.StatusCode)
	}

	// A full disk refuses uploads with a clear status.
	_, full, _ := newTestAppWith(t, Options{MinFree: 1 << 60})
	fp := full.call("POST", "/api/dav", map[string]any{"username": "o", "dir": "v"}, 200)["password"].(string)
	if resp, body := dav(t, full.base, "PUT", "/dav/n.md", "o", fp, []byte("x"), nil); resp.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("PUT on a full disk = %d %s", resp.StatusCode, body)
	}
}

// Sync spaces are kept apart from the user's own files and reached through ?space=.
func TestSyncSpaces(t *testing.T) {
	_, c, dir := newTestApp(t)
	onDisk := func(rel string) string { return filepath.Join(dir, "files", filepath.FromSlash(rel)) }
	exists := func(rel string) bool { _, err := os.Stat(onDisk(rel)); return err == nil }
	paths := func(m map[string]any) string {
		var out []string
		for _, e := range m["entries"].([]any) {
			out = append(out, e.(map[string]any)["path"].(string))
		}
		return "|" + strings.Join(out, "|") + "|"
	}

	for _, bad := range []string{"../x", "a/b", trashDir, syncDir, ".."} {
		if resp, _ := c.req("POST", "/api/dav", map[string]any{"username": "x", "space": bad}, true); resp.StatusCode != 400 {
			t.Errorf("space name %q = %d, want 400", bad, resp.StatusCode)
		}
	}
	made := c.call("POST", "/api/dav", map[string]any{"username": "sync1", "space": "vault"}, 200)
	pass := made["password"].(string)
	if key := made["key"].(map[string]any); key["space"] != "vault" || key["dir"] != "" {
		t.Fatalf("space key = %v", key)
	}

	// A sync client fills the space.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	dav(t, c.base, "PUT", "/dav/note.md", "sync1", pass, []byte("# note"), nil)
	dav(t, c.base, "PUT", "/dav/pic.png", "sync1", pass, png, nil)
	if !exists(syncDir + "/vault/note.md") {
		t.Fatal("the note is not stored in the space folder")
	}
	// The user's own file, to tell the two areas apart.
	c.req("POST", "/api/files/upload?dir=&name=mine.png", png, true)

	// Nothing of the space leaks into the normal views.
	for _, q := range []string{"/api/files/list?path=", "/api/files/find?q=note", "/api/files/find?q=pic", "/api/files/find?type=image", "/api/files/find?recent=1"} {
		got := paths(c.call("GET", q, nil, 200))
		if strings.Contains(got, "note.md") || strings.Contains(got, "pic.png") || strings.Contains(got, syncDir) {
			t.Errorf("%s shows space content: %s", q, got)
		}
	}
	for _, p := range []string{"/api/files/list?path=" + syncDir, "/api/files/download?path=" + syncDir + "/vault/note.md"} {
		if resp, _ := c.req("GET", p, nil, false); resp.StatusCode == 200 {
			t.Errorf("%s reached the space without ?space=", p)
		}
	}
	if resp, _ := c.req("GET", "/api/files/download?path=note.md", nil, false); resp.StatusCode != 404 {
		t.Errorf("space file is downloadable from the normal area: %d", resp.StatusCode)
	}

	// Inside the space the same file API works, with paths relative to the space.
	if got := paths(c.call("GET", "/api/files/list?path=&space=vault", nil, 200)); got != "|note.md|pic.png|" && got != "|pic.png|note.md|" {
		t.Errorf("space listing = %s", got)
	}
	if got := paths(c.call("GET", "/api/files/find?q=NOTE&space=vault", nil, 200)); got != "|note.md|" {
		t.Errorf("search inside the space = %s", got)
	}
	if got := paths(c.call("GET", "/api/files/find?type=image&space=vault", nil, 200)); got != "|pic.png|" {
		t.Errorf("images inside the space = %s (the user's own mine.png must not be here)", got)
	}
	if resp, data := c.req("GET", "/api/files/download?path=note.md&space=vault", nil, false); resp.StatusCode != 200 || string(data) != "# note" {
		t.Errorf("download inside the space = %d %q", resp.StatusCode, data)
	}
	c.call("POST", "/api/files/mkdir?space=vault", map[string]string{"dir": "", "name": "sub"}, 200)
	if resp, data := c.req("POST", "/api/files/upload?dir=sub&name=up.txt&space=vault", []byte("up"), true); resp.StatusCode != 200 || !strings.Contains(string(data), `"path":"sub/up.txt"`) {
		t.Fatalf("upload into the space = %d %s", resp.StatusCode, data)
	}
	c.call("POST", "/api/files/rename?space=vault", map[string]string{"path": "sub/up.txt", "name": "renamed.txt"}, 200)
	if !exists(syncDir+"/vault/sub/renamed.txt") || exists("sub") {
		t.Error("upload, mkdir or rename did not stay inside the space")
	}
	c.call("POST", "/api/files/delete?space=vault", map[string]any{"paths": []string{"sub/renamed.txt"}}, 200)
	trash := c.call("GET", "/api/trash", nil, 200)["items"].([]any)
	if len(trash) != 1 || trash[0].(map[string]any)["origPath"] != syncDir+"/vault/sub/renamed.txt" {
		t.Fatalf("trash after deleting inside the space = %v", trash)
	}
	c.call("POST", "/api/trash/restore", map[string]any{"ids": []string{trash[0].(map[string]any)["id"].(string)}}, 200)
	if !exists(syncDir + "/vault/sub/renamed.txt") {
		t.Error("restore did not put the file back into the space")
	}
	for _, bad := range []string{"..", trashDir, "a%2Fb", syncDir} {
		if resp, _ := c.req("GET", "/api/files/list?path=&space="+bad, nil, false); resp.StatusCode == 200 {
			t.Errorf("space=%s was accepted", bad)
		}
	}

	// A key for all files does not see the spaces either.
	all := c.call("POST", "/api/dav", map[string]any{"username": "all", "dir": ""}, 200)["password"].(string)
	if _, body := dav(t, c.base, "PROPFIND", "/dav/", "all", all, nil, map[string]string{"Depth": "1"}); strings.Contains(body, "mynas-sync") {
		t.Error("the spaces folder is visible over WebDAV to an all-files key")
	}

	spaces := c.call("GET", "/api/spaces", nil, 200)["spaces"].([]any)
	if len(spaces) != 1 || spaces[0].(map[string]any)["name"] != "vault" || fmt.Sprint(spaces[0].(map[string]any)["keys"]) != "[sync1]" || spaces[0].(map[string]any)["entries"] != float64(3) {
		t.Errorf("spaces = %v", spaces)
	}
	if resp, _ := c.req("DELETE", "/api/spaces/vault", nil, true); resp.StatusCode != 409 {
		t.Errorf("deleting a space that a key still uses = %d, want 409", resp.StatusCode)
	}

	// An existing folder key moves into a space; its clients keep working unchanged.
	old := c.call("POST", "/api/dav", map[string]any{"username": "old", "dir": "Notes/Obsidian"}, 200)
	oldPass, oldID := old["password"].(string), old["key"].(map[string]any)["id"].(string)
	c.call("POST", "/api/dav", map[string]any{"username": "old2", "dir": "Notes/Obsidian"}, 200)
	dav(t, c.base, "PUT", "/dav/a.md", "old", oldPass, []byte("before the move"), nil)
	if !exists("Notes/Obsidian/a.md") {
		t.Fatal("folder key did not write into its folder")
	}
	moved := c.call("POST", "/api/dav/"+oldID+"/to-space", nil, 200)
	if moved["space"] != "Obsidian" || exists("Notes/Obsidian") || !exists(syncDir+"/Obsidian/a.md") {
		t.Fatalf("to-space = %v, old folder still there: %v", moved, exists("Notes/Obsidian"))
	}
	if resp, body := dav(t, c.base, "GET", "/dav/a.md", "old", oldPass, nil, nil); resp.StatusCode != 200 || body != "before the move" {
		t.Errorf("client after the move: GET = %d %q", resp.StatusCode, body)
	}
	dav(t, c.base, "PUT", "/dav/b.md", "old", oldPass, []byte("after"), nil)
	if !exists(syncDir+"/Obsidian/b.md") || exists("Notes/Obsidian/b.md") {
		t.Error("after the move the client does not write into the space")
	}
	for _, it := range c.call("GET", "/api/dav", nil, 200)["keys"].([]any) {
		k := it.(map[string]any)
		if (k["username"] == "old" || k["username"] == "old2") && k["space"] != "Obsidian" {
			t.Errorf("key %v did not follow the folder into the space", k)
		}
	}
	if resp, _ := c.req("POST", "/api/dav/"+oldID+"/to-space", nil, true); resp.StatusCode != 400 {
		t.Errorf("moving a space key again = %d, want 400", resp.StatusCode)
	}

	// Once no key uses a space it can be removed; it goes to the recycle bin.
	for _, it := range c.call("GET", "/api/dav", nil, 200)["keys"].([]any) {
		if k := it.(map[string]any); k["username"] == "sync1" {
			c.call("DELETE", "/api/dav/"+k["id"].(string), nil, 200)
		}
	}
	c.call("DELETE", "/api/spaces/vault", nil, 200)
	if exists(syncDir + "/vault") {
		t.Error("the removed space is still there")
	}
	var id string
	for _, it := range c.call("GET", "/api/trash", nil, 200)["items"].([]any) {
		if m := it.(map[string]any); m["origPath"] == syncDir+"/vault" {
			id = m["id"].(string)
		}
	}
	if id == "" {
		t.Fatal("the removed space is not in the recycle bin")
	}
	c.call("POST", "/api/trash/restore", map[string]any{"ids": []string{id}}, 200)
	if !exists(syncDir + "/vault/note.md") {
		t.Error("restoring the space from the recycle bin failed")
	}
}

// The files directory can be changed from the web UI while the server runs.
func TestChangeFilesDir(t *testing.T) {
	app, c, dir := newTestApp(t)
	host := app.host
	t.Cleanup(host.Close)
	const pw = "correct horse"
	old := filepath.Join(dir, "files")
	other := filepath.Join(t.TempDir(), "新位置") // not there yet; its parent is
	set := func(d, password string) (int, string) {
		resp, data := c.req("PUT", "/api/settings/files-dir", map[string]string{"dir": d, "password": password}, true)
		return resp.StatusCode, string(data)
	}
	inUse := func() string { return c.call("GET", "/api/settings", nil, 200)["filesDir"].(string) }
	listed := func(q string) string {
		var out []string
		for _, e := range c.call("GET", q, nil, 200)["entries"].([]any) {
			out = append(out, e.(map[string]any)["path"].(string))
		}
		return strings.Join(out, "|")
	}

	// Content in the first directory: a file, a recycle bin item, a sync space and a proxied link.
	c.req("POST", "/api/files/upload?dir=&name=first.txt", []byte("first"), true)
	c.req("POST", "/api/files/upload?dir=&name=gone.txt", []byte("gone"), true)
	c.call("POST", "/api/files/delete", map[string]any{"paths": []string{"gone.txt"}}, 200)
	made := c.call("POST", "/api/dav", map[string]any{"username": "sync", "space": "vault"}, 200)
	davPass, keyID := made["password"].(string), made["key"].(map[string]any)["id"].(string)
	dav(t, c.base, "PUT", "/dav/note.md", "sync", davPass, []byte("my note"), nil)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "proxied") }))
	defer target.Close()
	port := int(c.call("POST", "/api/links", map[string]any{"name": "t", "url": target.URL, "mode": "embed", "proxy": true}, 200)["proxyPort"].(float64))
	if inUse() != old {
		t.Fatalf("files directory = %q, want %q", inUse(), old)
	}

	// What must be refused.
	sys := "/etc"
	if runtime.GOOS == "windows" {
		sys = os.Getenv("SystemRoot")
	}
	refused := map[string]string{
		"a relative path":                  "relative/dir",
		"the directory already in use":     old,
		"a place holding the config":       dir,
		"a folder whose parent is missing": filepath.Join(t.TempDir(), "no", "such"),
		"a file":                           filepath.Join(old, "first.txt"),
		"a system directory":               sys,
	}
	for what, d := range refused {
		if code, body := set(d, pw); code != 400 {
			t.Errorf("%s (%s) = %d %s, want 400", what, d, code, body)
		}
	}
	if code, body := set(other, "wrong password"); code != 403 || !strings.Contains(body, "密码") {
		t.Errorf("wrong password = %d %s, want 403", code, body)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a refused change still created the directory")
	}

	// A running download blocks the change.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "400000")
		for i := 0; i < 40; i++ {
			if _, err := w.Write(make([]byte, 10000)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer slow.Close()
	c.call("POST", "/api/downloads", map[string]any{"urls": []string{slow.URL + "/big.bin"}}, 200)
	d := waitDownload(t, c, "", func(d map[string]any) bool { return d["status"] == "running" })
	if code, body := set(other, pw); code != 409 {
		t.Errorf("change during a download = %d %s, want 409", code, body)
	}
	c.call("POST", "/api/downloads/"+d["id"].(string)+"/cancel", nil, 200)
	waitDownload(t, c, d["id"].(string), func(d map[string]any) bool { return d["status"] == "canceled" })

	// The change itself. The same login keeps working.
	if code, body := set(other, pw); code != 200 {
		t.Fatalf("change = %d %s", code, body)
	}
	if inUse() != other || c.call("GET", "/api/storage", nil, 200)["dir"] != other {
		t.Fatalf("after the change the directory in use is %q", inUse())
	}
	if got := listed("/api/files/list?path="); got != "" {
		t.Errorf("new directory lists %q, want nothing", got)
	}
	if got := listed("/api/files/find?q=first"); got != "" {
		t.Errorf("search still finds old files: %q", got)
	}
	if n := len(c.call("GET", "/api/trash", nil, 200)["items"].([]any)); n != 0 {
		t.Errorf("recycle bin shows %d items of the old directory", n)
	}
	c.req("POST", "/api/files/upload?dir=&name=second.txt", []byte("second"), true)
	if b, _ := os.ReadFile(filepath.Join(other, "second.txt")); string(b) != "second" {
		t.Error("upload after the change did not land in the new directory")
	}
	if b, _ := os.ReadFile(filepath.Join(old, "first.txt")); string(b) != "first" {
		t.Error("the old directory was touched")
	}
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !bytes.Contains(state, []byte(`"filesDir"`)) {
		t.Error("the chosen directory is not saved")
	}
	proxy := &client{t: t, base: "http://127.0.0.1:" + strconv.Itoa(port), cookie: c.cookie}
	if resp, data := proxy.req("GET", "/", nil, false); resp.StatusCode != 200 || string(data) != "proxied" {
		t.Errorf("link proxy after the change = %d %q", resp.StatusCode, data)
	}

	// A sync client must get an error, never an empty remote it would mirror by deleting local files.
	if resp, _ := dav(t, c.base, "PROPFIND", "/dav/", "sync", davPass, nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("WebDAV with the space missing = %d, want 503", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(other, syncDir)); err == nil {
		t.Error("the missing space was created silently")
	}
	keyOf := func() map[string]any {
		for _, it := range c.call("GET", "/api/dav", nil, 200)["keys"].([]any) {
			if k := it.(map[string]any); k["id"] == keyID {
				return k
			}
		}
		return nil
	}
	if keyOf()["missing"] != true {
		t.Errorf("key = %v, want missing", keyOf())
	}
	c.call("POST", "/api/dav/"+keyID+"/init", nil, 200)
	if resp, _ := dav(t, c.base, "PUT", "/dav/new.md", "sync", davPass, []byte("x"), nil); resp.StatusCode != 201 || keyOf()["missing"] != false {
		t.Errorf("after creating the folder on purpose: PUT = %d, key = %v", resp.StatusCode, keyOf())
	}

	// A restart keeps the choice although the command line still names the default.
	lo, hi := freePortRange(t)
	again, err := NewHost(Options{DataDir: dir, BindHost: "127.0.0.1", PortMin: lo, PortMax: hi})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.cur.Load().files.dir; got != other || again.warn != "" {
		t.Errorf("after a restart the directory is %q (warning %q), want %q", got, again.warn, other)
	}
	again.Close()

	// Back to the first directory: its files, recycle bin and sync space are all there again.
	if code, body := set(old, pw); code != 200 {
		t.Fatalf("change back = %d %s", code, body)
	}
	// (the cancelled download above left its 离线下载 folder here as well)
	if got := "|" + listed("/api/files/list?path=") + "|"; !strings.Contains(got, "|first.txt|") || strings.Contains(got, "second.txt") {
		t.Errorf("first directory lists %q", got)
	}
	// The directory that is no longer in use is released: it can be removed.
	if err := os.RemoveAll(other); err != nil {
		t.Errorf("the directory left behind is still held open: %v", err)
	}
	trash := c.call("GET", "/api/trash", nil, 200)["items"].([]any)
	if len(trash) != 1 {
		t.Fatalf("recycle bin of the first directory has %d items, want 1", len(trash))
	}
	c.call("POST", "/api/trash/restore", map[string]any{"ids": []string{trash[0].(map[string]any)["id"].(string)}}, 200)
	if b, _ := os.ReadFile(filepath.Join(old, "gone.txt")); string(b) != "gone" {
		t.Error("restore after changing back failed")
	}
	if resp, body := dav(t, c.base, "GET", "/dav/note.md", "sync", davPass, nil, nil); resp.StatusCode != 200 || body != "my note" {
		t.Errorf("sync space after changing back: GET = %d %q", resp.StatusCode, body)
	}
}

// With a hook (the OpenWrt package) the choice goes to the hook, not to state.json.
func TestFilesDirHook(t *testing.T) {
	app, c, dir := newTestApp(t)
	host := app.host
	t.Cleanup(host.Close)
	var got []string
	fail := false
	host.hook = func(d string) error {
		if fail {
			return fmt.Errorf("uci: permission denied")
		}
		got = append(got, d)
		return nil
	}
	set := func(d string) int {
		resp, _ := c.req("PUT", "/api/settings/files-dir", map[string]string{"dir": d, "password": "correct horse"}, true)
		return resp.StatusCode
	}
	one, two := t.TempDir(), t.TempDir()
	if code := set(one); code != 200 || len(got) != 1 || got[0] != one {
		t.Fatalf("change with a hook = %d, hook got %v", code, got)
	}
	if s := c.call("GET", "/api/settings", nil, 200); s["system"] != true || s["filesDir"] != one {
		t.Errorf("settings = %v", s)
	}
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(state, []byte(`"filesDir"`)) {
		t.Error("with a hook the directory must not be stored in state.json as well")
	}
	// When the hook fails nothing changes.
	fail = true
	if code := set(two); code != 500 {
		t.Errorf("change with a failing hook = %d, want 500", code)
	}
	if dirNow := c.call("GET", "/api/settings", nil, 200)["filesDir"]; dirNow != one {
		t.Errorf("after a failed change the directory is %v, want %s", dirNow, one)
	}
}

// When the saved directory is gone at start (a disk not plugged in), the
// server still comes up, says so, and sync clients are not shown an empty remote.
func TestSavedFilesDirUnusable(t *testing.T) {
	app, c, dir := newTestApp(t)
	pass := c.call("POST", "/api/dav", map[string]any{"username": "sync", "space": "vault"}, 200)["password"].(string)
	disk := filepath.Join(t.TempDir(), "disk")
	if resp, data := c.req("PUT", "/api/settings/files-dir", map[string]string{"dir": disk, "password": "correct horse"}, true); resp.StatusCode != 200 {
		t.Fatalf("change = %d %s", resp.StatusCode, data)
	}
	app.host.Close()
	if err := os.RemoveAll(disk); err != nil {
		t.Fatal(err)
	}

	lo, hi := freePortRange(t)
	h, err := NewHost(Options{DataDir: dir, BindHost: "127.0.0.1", PortMin: lo, PortMax: hi})
	if err != nil {
		t.Fatalf("the server does not start without the saved directory: %v", err)
	}
	defer h.Close()
	if got := h.cur.Load().files.dir; got != filepath.Join(dir, "files") || !strings.Contains(h.warn, disk) {
		t.Errorf("fallback directory %q, warning %q", got, h.warn)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if resp, _ := dav(t, srv.URL, "PROPFIND", "/dav/", "sync", pass, nil, nil); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		// The space exists in the default directory from before the change, so this client is fine.
		t.Errorf("WebDAV on the fallback directory = %d", resp.StatusCode)
	}
}

// Recycle bin records from before 0.1.6 have no directory; they belong to the one in use.
func TestLegacyTrashRecords(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "files", trashDir), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "files", trashDir, "t1"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"trash":[{"id":"t1","name":"old.txt","origPath":"old.txt","deletedAt":"`+time.Now().Format(time.RFC3339)+`"}],"seq":1,"settings":{"trashDays":7}}`), 0o600)
	lo, hi := freePortRange(t)
	h, err := NewHost(Options{DataDir: dir, BindHost: "127.0.0.1", PortMin: lo, PortMax: hi})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if len(h.st.Trash) != 1 || h.st.Trash[0].Home != filepath.Join(dir, "files") {
		t.Fatalf("legacy record = %+v", h.st.Trash)
	}
	if h.st.trashDays() != 7 {
		t.Errorf("trash retention read as %d, want 7", h.st.trashDays())
	}
}

func TestHostDirs(t *testing.T) {
	_, c, _ := newTestApp(t)
	base := t.TempDir()
	for _, d := range []string{"照片", "b", ".hidden"} {
		os.Mkdir(filepath.Join(base, d), 0o755)
	}
	os.WriteFile(filepath.Join(base, "file.txt"), []byte("x"), 0o644)
	got := c.call("GET", "/api/hostdirs?path="+url.QueryEscape(base), nil, 200)
	if fmt.Sprint(got["dirs"]) != "[b 照片]" || got["path"] != base || got["parent"] != filepath.Dir(base) {
		t.Errorf("hostdirs = %v", got)
	}
	if roots := c.call("GET", "/api/hostdirs?path=", nil, 200)["roots"].([]any); len(roots) == 0 {
		t.Error("no starting points for browsing")
	}
	if resp, _ := c.req("GET", "/api/hostdirs?path=relative", nil, false); resp.StatusCode != 400 {
		t.Errorf("relative path = %d, want 400", resp.StatusCode)
	}
	anon := &client{t: t, base: c.base}
	if resp, _ := anon.req("GET", "/api/hostdirs?path="+url.QueryEscape(base), nil, false); resp.StatusCode != 401 {
		t.Errorf("anonymous = %d, want 401", resp.StatusCode)
	}
}

func TestStatePersists(t *testing.T) {
	dir := t.TempDir()
	lo, hi := freePortRange(t)
	opts := Options{DataDir: dir, BindHost: "127.0.0.1", PortMin: lo, PortMax: hi}
	app, err := NewApp(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	c := &client{t: t, base: srv.URL}
	resp, _ := c.req("POST", "/api/setup", map[string]string{"username": "admin", "password": "12345678"}, true)
	c.cookie = resp.Cookies()[0]
	c.call("POST", "/api/links", map[string]any{"name": "文档", "url": "https://example.com/docs", "mode": "new", "pin": true}, 200)
	srv.Close()
	app.files.root.Close()

	app2, err := NewApp(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.files.root.Close()
	if len(app2.st.Links) != 1 || app2.st.Links[0].Name != "文档" || app2.st.Admin == nil {
		t.Errorf("state after restart: %+v", app2.st.State)
	}
}

func TestAccount(t *testing.T) {
	_, c, dir := newTestApp(t)
	login := func(user, pass string) *client {
		o := &client{t: t, base: c.base}
		resp, _ := o.req("POST", "/api/login", map[string]string{"username": user, "password": pass}, true)
		if resp.StatusCode != 200 {
			return nil
		}
		for _, ck := range resp.Cookies() {
			if ck.Name == cookieName {
				o.cookie = ck
			}
		}
		return o
	}
	other := login("admin", "correct horse")
	if other == nil {
		t.Fatal("second browser could not log in")
	}
	if got := c.call("GET", "/api/settings", nil, 200)["username"]; got != "admin" {
		t.Errorf("settings username = %v, want admin", got)
	}

	anon := &client{t: t, base: c.base}
	if resp, _ := anon.req("PUT", "/api/account", map[string]string{"password": "correct horse", "newPassword": "another pass"}, true); resp.StatusCode != 401 {
		t.Errorf("anonymous change = %d, want 401", resp.StatusCode)
	}
	if resp, _ := c.req("PUT", "/api/account", map[string]string{"password": "correct horse", "newPassword": "another pass"}, false); resp.StatusCode != 403 {
		t.Errorf("change without the header = %d, want 403", resp.StatusCode)
	}
	c.call("PUT", "/api/account", map[string]string{"password": "wrong", "newPassword": "another pass"}, 403)
	c.call("PUT", "/api/account", map[string]string{"password": "correct horse", "newPassword": "short"}, 400)
	c.call("PUT", "/api/account", map[string]string{"password": "correct horse", "username": strings.Repeat("a", 33)}, 400)
	c.call("PUT", "/api/account", map[string]string{"password": "correct horse"}, 400)
	c.call("PUT", "/api/account", map[string]string{"password": "correct horse", "username": "admin"}, 400)
	if login("admin", "correct horse") == nil {
		t.Fatal("refused changes must leave the account alone")
	}
	other = login("admin", "correct horse")

	// Password only: the name stays, this session stays, the other one is signed out.
	out := c.call("PUT", "/api/account", map[string]string{"password": "correct horse", "newPassword": "another pass"}, 200)
	if out["username"] != "admin" || out["passwordChanged"] != true {
		t.Errorf("reply = %v", out)
	}
	c.call("GET", "/api/files/list", nil, 200)
	if resp, _ := other.req("GET", "/api/files/list", nil, true); resp.StatusCode != 401 {
		t.Errorf("other session after the change = %d, want 401", resp.StatusCode)
	}
	if login("admin", "correct horse") != nil {
		t.Error("the old password still works")
	}
	if login("admin", "another pass") == nil {
		t.Error("the new password does not work")
	}
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(state, []byte("another pass")) {
		t.Error("state.json contains the plain password")
	}

	// Username only: the password stays.
	out = c.call("PUT", "/api/account", map[string]string{"password": "another pass", "username": "  家里  "}, 200)
	if out["username"] != "家里" || out["passwordChanged"] != false {
		t.Errorf("reply = %v", out)
	}
	if login("admin", "another pass") != nil {
		t.Error("the old username still works")
	}
	if login("家里", "another pass") == nil {
		t.Error("the new username does not work")
	}
	// The old password no longer confirms anything.
	c.call("PUT", "/api/account", map[string]string{"password": "correct horse", "newPassword": "third password"}, 403)

	// Wrong guesses here count towards the same lockout as the login page.
	for i := 0; i < maxFails; i++ {
		c.req("PUT", "/api/account", map[string]string{"password": "nope", "newPassword": "third password"}, true)
	}
	c.call("PUT", "/api/account", map[string]string{"password": "another pass", "newPassword": "third password"}, 429)
}

func TestWebDAVNewPassword(t *testing.T) {
	_, c, dir := newTestApp(t)
	created := c.call("POST", "/api/dav", map[string]any{"username": "obsidian", "space": "Obsidian"}, 200)
	oldPass := created["password"].(string)
	id := created["key"].(map[string]any)["id"].(string)
	if resp, _ := dav(t, c.base, "PUT", "/dav/a.md", "obsidian", oldPass, []byte("note"), nil); resp.StatusCode != 201 {
		t.Fatalf("put with the first password = %d", resp.StatusCode)
	}

	c.call("POST", "/api/dav/nope/password", nil, 404)
	anon := &client{t: t, base: c.base}
	if resp, _ := anon.req("POST", "/api/dav/"+id+"/password", nil, true); resp.StatusCode != 401 {
		t.Errorf("anonymous = %d, want 401", resp.StatusCode)
	}
	out := c.call("POST", "/api/dav/"+id+"/password", nil, 200)
	newPass, _ := out["password"].(string)
	key := out["key"].(map[string]any)
	if len(newPass) < 30 || newPass == oldPass || key["username"] != "obsidian" || key["space"] != "Obsidian" || key["id"] != id {
		t.Fatalf("reply = %v", out)
	}
	if resp, _ := dav(t, c.base, "GET", "/dav/a.md", "obsidian", oldPass, nil, nil); resp.StatusCode != 401 {
		t.Errorf("old password = %d, want 401", resp.StatusCode)
	}
	if resp, body := dav(t, c.base, "GET", "/dav/a.md", "obsidian", newPass, nil, nil); resp.StatusCode != 200 || body != "note" {
		t.Errorf("new password = %d %q", resp.StatusCode, body)
	}
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(state, []byte(newPass)) {
		t.Error("state.json contains the plain access password")
	}
	keys := c.call("GET", "/api/dav", nil, 200)["keys"].([]any)
	if len(keys) != 1 {
		t.Errorf("keys after a new password: %v", keys)
	}
}
