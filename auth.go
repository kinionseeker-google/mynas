package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	cookieName  = "mynas_session"
	sessionTTL  = 7 * 24 * time.Hour
	pbkdf2Iter  = 600_000
	maxFails    = 5
	lockoutTime = time.Minute
)

type failRec struct {
	count int
	until time.Time
}

// Auth handles the admin account, sessions and login throttling.
type Auth struct {
	st       *Store
	mu       sync.Mutex
	sessions map[string]time.Time
	fails    map[string]*failRec
}

func NewAuth(st *Store) *Auth {
	return &Auth{st: st, sessions: map[string]time.Time{}, fails: map[string]*failRec{}}
}

func hashPassword(pw string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pw, salt, iter, 32)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// valid reports whether the request carries a live session cookie.
func (a *Auth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func (a *Auth) startSession(w http.ResponseWriter, r *http.Request) {
	token := randomHex(32)
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()),
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// require wraps handlers that need a logged-in admin.
func (a *Auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			writeErr(w, http.StatusUnauthorized, "请先登录")
			return
		}
		next(w, r)
	}
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *Auth) handleSession(w http.ResponseWriter, r *http.Request) {
	a.st.mu.Lock()
	admin := a.st.Admin
	a.st.mu.Unlock()
	out := map[string]any{"setupDone": admin != nil, "authed": false}
	if admin != nil && a.valid(r) {
		out["authed"] = true
		out["username"] = admin.Username
	}
	writeJSON(w, out)
}

func (a *Auth) handleSetup(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !readJSON(w, r, &c) {
		return
	}
	c.Username = strings.TrimSpace(c.Username)
	if c.Username == "" || len(c.Username) > 32 {
		writeErr(w, http.StatusBadRequest, "请填写用户名（不超过 32 个字符）")
		return
	}
	if len(c.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "密码至少 8 位")
		return
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密码盐失败")
		return
	}
	hash, err := hashPassword(c.Password, salt, pbkdf2Iter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存密码失败")
		return
	}
	a.st.mu.Lock()
	if a.st.Admin != nil {
		a.st.mu.Unlock()
		writeErr(w, http.StatusConflict, "管理员账号已经创建，请直接登录")
		return
	}
	a.st.Admin = &Admin{Username: c.Username, Salt: salt, Hash: hash, Iter: pbkdf2Iter}
	err = a.st.save()
	a.st.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存账号失败: "+err.Error())
		return
	}
	a.startSession(w, r)
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	a.mu.Lock()
	if f := a.fails[ip]; f != nil && time.Now().Before(f.until) {
		a.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "尝试次数过多，请 1 分钟后再试")
		return
	}
	a.mu.Unlock()

	var c credentials
	if !readJSON(w, r, &c) {
		return
	}
	a.st.mu.Lock()
	admin := a.st.Admin
	a.st.mu.Unlock()
	if admin == nil {
		writeErr(w, http.StatusConflict, "还没有创建管理员账号")
		return
	}
	hash, err := hashPassword(c.Password, admin.Salt, admin.Iter)
	userOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(c.Username)), []byte(admin.Username)) == 1
	passOK := err == nil && subtle.ConstantTimeCompare(hash, admin.Hash) == 1
	if !userOK || !passOK {
		a.mu.Lock()
		f := a.fails[ip]
		if f == nil {
			f = &failRec{}
			a.fails[ip] = f
		}
		f.count++
		if f.count >= maxFails {
			f.count = 0
			f.until = time.Now().Add(lockoutTime)
		}
		a.mu.Unlock()
		writeErr(w, http.StatusUnauthorized, "用户名或密码不正确")
		return
	}
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
	a.startSession(w, r)
	writeJSON(w, map[string]any{"ok": true})
}

// confirmPassword asks for the admin password again before a sensitive
// action. It shares the login throttle and answers the request itself when
// the password is wrong. A wrong password is 403, not 401: the session is fine.
func (a *Auth) confirmPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := clientIP(r)
	a.mu.Lock()
	if f := a.fails[ip]; f != nil && time.Now().Before(f.until) {
		a.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "尝试次数过多，请 1 分钟后再试")
		return false
	}
	a.mu.Unlock()
	a.st.mu.Lock()
	admin := a.st.Admin
	a.st.mu.Unlock()
	if admin == nil {
		writeErr(w, http.StatusConflict, "还没有创建管理员账号")
		return false
	}
	hash, err := hashPassword(password, admin.Salt, admin.Iter)
	if err != nil || subtle.ConstantTimeCompare(hash, admin.Hash) != 1 {
		a.mu.Lock()
		f := a.fails[ip]
		if f == nil {
			f = &failRec{}
			a.fails[ip] = f
		}
		if f.count++; f.count >= maxFails {
			f.count, f.until = 0, time.Now().Add(lockoutTime)
		}
		a.mu.Unlock()
		writeErr(w, http.StatusForbidden, "管理员密码不正确")
		return false
	}
	return true
}

// handleAccount changes the admin's username and/or password. The current
// password is asked again, and every other session is signed out afterwards:
// whoever changes the password usually wants other browsers locked out.
func (a *Auth) handleAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password    string `json:"password"`
		Username    string `json:"username"`
		NewPassword string `json:"newPassword"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !a.confirmPassword(w, r, in.Password) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if len(in.Username) > 32 {
		writeErr(w, http.StatusBadRequest, "用户名不超过 32 个字符")
		return
	}
	var salt, hash []byte
	if in.NewPassword != "" {
		if len(in.NewPassword) < 8 {
			writeErr(w, http.StatusBadRequest, "新密码至少 8 位")
			return
		}
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			writeErr(w, http.StatusInternalServerError, "生成密码盐失败")
			return
		}
		var err error
		if hash, err = hashPassword(in.NewPassword, salt, pbkdf2Iter); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存密码失败")
			return
		}
	}

	a.st.mu.Lock()
	if a.st.Admin == nil {
		a.st.mu.Unlock()
		writeErr(w, http.StatusConflict, "还没有创建管理员账号")
		return
	}
	old := *a.st.Admin
	next := old
	if in.Username != "" {
		next.Username = in.Username
	}
	if hash != nil {
		next.Salt, next.Hash, next.Iter = salt, hash, pbkdf2Iter
	}
	if next.Username == old.Username && hash == nil {
		a.st.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "没有要修改的内容")
		return
	}
	a.st.Admin = &next
	if err := a.st.save(); err != nil {
		a.st.Admin = &old
		a.st.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "保存账号失败: "+err.Error())
		return
	}
	a.st.mu.Unlock()

	keep := ""
	if c, err := r.Cookie(cookieName); err == nil {
		keep = c.Value
	}
	a.mu.Lock()
	for token := range a.sessions {
		if token != keep {
			delete(a.sessions, token)
		}
	}
	delete(a.fails, clientIP(r))
	a.mu.Unlock()
	writeJSON(w, map[string]any{"username": next.Username, "passwordChanged": hash != nil})
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

// ---- small JSON helpers shared by all handlers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求内容格式不正确")
		return false
	}
	return true
}
