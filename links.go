package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Links serves the external links and runs one reverse-proxy listener per proxied link.
//
// A proxied link gets its own port instead of a path prefix: the target keeps
// its own root, so absolute paths, redirects and websockets work unchanged.
type Links struct {
	st       *Store
	auth     *Auth
	bindHost string
	portMin  int
	portMax  int

	mu      sync.Mutex
	servers map[string]*http.Server // by link id
}

func NewLinks(st *Store, auth *Auth, bindHost string, portMin, portMax int) *Links {
	return &Links{st: st, auth: auth, bindHost: bindHost, portMin: portMin, portMax: portMax, servers: map[string]*http.Server{}}
}

// parseHTTPURL accepts only absolute http/https URLs.
func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("链接地址需要以 http:// 或 https:// 开头")
	}
	if u.User != nil {
		return nil, errors.New("链接地址里不要包含账号密码")
	}
	return u, nil
}

func normalizeLink(l *Link) error {
	l.Name = strings.TrimSpace(l.Name)
	l.Desc = strings.TrimSpace(l.Desc)
	if l.Name == "" || len([]rune(l.Name)) > 30 {
		return errors.New("请填写名称（不超过 30 个字）")
	}
	if len([]rune(l.Desc)) > 80 {
		return errors.New("说明不超过 80 个字")
	}
	u, err := parseHTTPURL(l.URL)
	if err != nil {
		return err
	}
	l.URL = u.String()
	if l.Mode != "embed" && l.Mode != "new" {
		return errors.New("打开方式不正确")
	}
	if l.Mode != "embed" {
		l.Proxy = false
	}
	return nil
}

// Start launches the proxies for links saved earlier.
func (ls *Links) Start() {
	ls.st.mu.Lock()
	links := make([]Link, 0, len(ls.st.Links))
	for _, l := range ls.st.Links {
		links = append(links, *l)
	}
	ls.st.mu.Unlock()
	for _, l := range links {
		if l.Proxy && l.ProxyPort != 0 {
			if err := ls.startProxy(l); err != nil {
				log.Printf("链接“%s”的代理端口 %d 启动失败: %v", l.Name, l.ProxyPort, err)
			}
		}
	}
}

// StopAll closes every proxy listener, freeing the ports for a new App.
func (ls *Links) StopAll() {
	ls.mu.Lock()
	servers := ls.servers
	ls.servers = map[string]*http.Server{}
	ls.mu.Unlock()
	for _, srv := range servers {
		_ = srv.Close()
	}
}

func (ls *Links) stopProxy(id string) {
	ls.mu.Lock()
	srv := ls.servers[id]
	delete(ls.servers, id)
	ls.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

func (ls *Links) startProxy(l Link) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(ls.bindHost, strconv.Itoa(l.ProxyPort)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: ls.proxyHandler(l), ReadHeaderTimeout: 15 * time.Second}
	ls.mu.Lock()
	ls.servers[l.ID] = srv
	ls.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// allocPort picks a free port in the range and starts the proxy on it.
func (ls *Links) allocAndStart(l *Link, used map[int]bool) error {
	for p := ls.portMin; p <= ls.portMax; p++ {
		if used[p] {
			continue
		}
		l.ProxyPort = p
		if err := ls.startProxy(*l); err == nil {
			return nil
		}
	}
	l.ProxyPort = 0
	return fmt.Errorf("代理端口 %d-%d 已用完或被占用", ls.portMin, ls.portMax)
}

// apply (re)starts or stops the proxy so it matches the link. The caller holds st.mu.
func (ls *Links) apply(l *Link) error {
	ls.stopProxy(l.ID)
	if !l.Proxy {
		l.ProxyPort = 0
		return nil
	}
	if l.ProxyPort != 0 {
		if err := ls.startProxy(*l); err == nil {
			return nil
		}
	}
	used := map[int]bool{}
	for _, o := range ls.st.Links {
		if o.ID != l.ID && o.ProxyPort != 0 {
			used[o.ProxyPort] = true
		}
	}
	return ls.allocAndStart(l, used)
}

func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

// stripFrameAncestors removes the frame-ancestors directive from a CSP value.
func stripFrameAncestors(csp string) string {
	parts := strings.Split(csp, ";")
	kept := parts[:0]
	for _, p := range parts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "frame-ancestors") {
			continue
		}
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ";")
}

func allowFraming(h http.Header) {
	h.Del("X-Frame-Options")
	for _, name := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		vals := h.Values(name)
		if len(vals) == 0 {
			continue
		}
		h.Del(name)
		for _, v := range vals {
			if v = stripFrameAncestors(v); v != "" {
				h.Add(name, v)
			}
		}
	}
}

func insecureTransport(insecure bool) *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func (ls *Links) proxyHandler(l Link) http.Handler {
	target, _ := url.Parse(l.URL)
	origin := target.Scheme + "://" + target.Host
	rp := &httputil.ReverseProxy{
		Transport: insecureTransport(l.Insecure),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: target.Scheme, Host: target.Host})
			// The NAS session cookie is sent to every port of this host; never forward it.
			stripCookie(pr.Out, cookieName)
			// Targets that check Origin/Referer should see themselves, not the NAS.
			if pr.Out.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", origin)
			}
			if ref, err := url.Parse(pr.Out.Header.Get("Referer")); err == nil && ref.Host == pr.In.Host {
				ref.Scheme, ref.Host = target.Scheme, target.Host
				pr.Out.Header.Set("Referer", ref.String())
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			allowFraming(resp.Header)
			if loc := resp.Header.Get("Location"); loc != "" {
				if u, err := url.Parse(loc); err == nil && u.Host == target.Host {
					u.Scheme, u.Host = "", ""
					resp.Header.Set("Location", u.String())
				}
			}
			if cookies := resp.Cookies(); len(cookies) > 0 {
				resp.Header.Del("Set-Cookie")
				// The proxy port speaks plain HTTP on the NAS host, so cookies
				// bound to the target's domain or to HTTPS would be dropped.
				for _, c := range cookies {
					c.Domain = ""
					c.Secure = false
					if c.SameSite == http.SameSiteNoneMode {
						c.SameSite = http.SameSiteLaxMode
					}
					resp.Header.Add("Set-Cookie", c.String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><body style="font:14px system-ui;padding:32px;color:#5b6676"><h3 style="color:#1f2937">无法连接到“%s”</h3><p>%s</p><p>%s</p></body>`,
				html.EscapeString(l.Name), html.EscapeString(origin), html.EscapeString(err.Error()))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ls.auth.valid(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><body style="font:14px system-ui;padding:32px;color:#5b6676">请先登录 NAS 管理页面，再打开这个链接。</body>`)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

func (ls *Links) handleList(w http.ResponseWriter, r *http.Request) {
	ls.st.mu.Lock()
	out := make([]Link, 0, len(ls.st.Links))
	for _, l := range ls.st.Links {
		out = append(out, *l)
	}
	ls.st.mu.Unlock()
	writeJSON(w, map[string]any{"links": out})
}

func (ls *Links) handleCreate(w http.ResponseWriter, r *http.Request) {
	var l Link
	if !readJSON(w, r, &l) {
		return
	}
	l.ProxyPort = 0
	if err := normalizeLink(&l); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ls.st.mu.Lock()
	defer ls.st.mu.Unlock()
	l.ID = ls.st.nextID("l")
	if err := ls.apply(&l); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	ls.st.Links = append(ls.st.Links, &l)
	if err := ls.st.save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, l)
}

func (ls *Links) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in Link
	if !readJSON(w, r, &in) {
		return
	}
	if err := normalizeLink(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	ls.st.mu.Lock()
	defer ls.st.mu.Unlock()
	for _, l := range ls.st.Links {
		if l.ID != id {
			continue
		}
		proxyChanged := l.URL != in.URL || l.Proxy != in.Proxy || l.Insecure != in.Insecure || l.Name != in.Name
		l.Name, l.URL, l.Desc, l.Mode, l.Pin, l.Proxy, l.Insecure = in.Name, in.URL, in.Desc, in.Mode, in.Pin, in.Proxy, in.Insecure
		if proxyChanged {
			if err := ls.apply(l); err != nil {
				l.Proxy = false
				_ = ls.st.save()
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}
		if err := ls.st.save(); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, *l)
		return
	}
	writeErr(w, http.StatusNotFound, "链接不存在")
}

func (ls *Links) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ls.st.mu.Lock()
	defer ls.st.mu.Unlock()
	for i, l := range ls.st.Links {
		if l.ID == id {
			ls.stopProxy(id)
			ls.st.Links = append(ls.st.Links[:i], ls.st.Links[i+1:]...)
			if err := ls.st.save(); err != nil {
				writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "链接不存在")
}

// framingBlocked reports whether the response headers forbid showing the page in a frame.
func framingBlocked(h http.Header) (bool, string) {
	if v := strings.ToLower(strings.TrimSpace(h.Get("X-Frame-Options"))); v != "" {
		return true, "X-Frame-Options: " + v
	}
	for _, csp := range h.Values("Content-Security-Policy") {
		for _, d := range strings.Split(csp, ";") {
			d = strings.ToLower(strings.TrimSpace(d))
			if strings.HasPrefix(d, "frame-ancestors") && !strings.Contains(d, "*") {
				return true, d
			}
		}
	}
	return false, ""
}

// handleCheck fetches the link from the NAS and reports whether it is reachable and embeddable.
func (ls *Links) handleCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ls.st.mu.Lock()
	var link *Link
	for _, l := range ls.st.Links {
		if l.ID == id {
			c := *l
			link = &c
		}
	}
	ls.st.mu.Unlock()
	if link == nil {
		writeErr(w, http.StatusNotFound, "链接不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "链接地址不正确")
		return
	}
	tr := insecureTransport(link.Insecure)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		writeJSON(w, map[string]any{"reachable": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	blocked, reason := framingBlocked(resp.Header)
	writeJSON(w, map[string]any{"reachable": true, "status": resp.StatusCode, "frameBlocked": blocked, "reason": reason})
}
