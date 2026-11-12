//go:build windows

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Спільний буфер обміну. Протокол VNC уміє передавати лише текст, і то не в кожному
// клієнті, тому текст і файли ходять через окрему сторінку /clip/ на порту
// веб-клієнта. Вона працює з будь-якого пристрою з браузером.
//
// Захист: вхід паролем VNC (пароль мережею не передається — лише його хеш з
// одноразовим числом), далі сеансова cookie. Завантажити з ПК можна тільки ті
// файли, які зараз лежать у буфері обміну, — не довільний шлях.

//go:embed clip.html
var clipHTML []byte

// Сценарій для Mac: автоматична синхронізація тексту штатними засобами macOS.
//
//go:embed clipmac.sh
var clipMacScript string

type clipHub struct {
	srv      *Server
	mu       sync.Mutex
	sessions map[string]time.Time
	nonces   map[string]time.Time
}

type clipFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

func randHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newClipHub(srv *Server, mux *http.ServeMux) *clipHub {
	h := &clipHub{srv: srv, sessions: map[string]time.Time{}, nonces: map[string]time.Time{}}
	mux.HandleFunc("/clip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/clip/", http.StatusFound)
	})
	mux.HandleFunc("/clip/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clip/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(clipHTML)
	})
	mux.HandleFunc("/clip/mac.sh", h.macScript)
	mux.HandleFunc("/clip/api/plain", h.guard(h.plain))
	mux.HandleFunc("/clip/api/nonce", h.nonce)
	mux.HandleFunc("/clip/api/login", h.login)
	mux.HandleFunc("/clip/api/state", h.guard(h.state))
	mux.HandleFunc("/clip/api/text", h.guard(h.text))
	mux.HandleFunc("/clip/api/upload", h.guard(h.upload))
	mux.HandleFunc("/clip/api/file", h.guard(h.file))
	return h
}

func clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// sameOrigin відсікає запити з чужих сайтів, відкритих у тому ж браузері.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (h *clipHub) authorized(r *http.Request) bool {
	if h.srv.password == "" {
		return true // сервер без пароля — сторінка теж відкрита
	}
	c, err := r.Cookie("svnc_clip")
	if err != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.sessions[c.Value]
	return ok && time.Now().Before(exp)
}

// reject відповідає відмовою, спершу дочитавши невеликий залишок запиту: інакше
// клієнт замість коду відповіді бачить обірване з'єднання.
func reject(w http.ResponseWriter, r *http.Request, msg string, code int) {
	io.CopyN(io.Discard, r.Body, 64<<10)
	http.Error(w, msg, code)
}

func (h *clipHub) guard(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !sameOrigin(r) {
			reject(w, r, "forbidden", http.StatusForbidden)
			return
		}
		if !h.authorized(r) {
			reject(w, r, "login required", http.StatusUnauthorized)
			return
		}
		f(w, r)
	}
}

// nonce видає одноразове число для входу й повідомляє, чи потрібен пароль.
func (h *clipHub) nonce(w http.ResponseWriter, r *http.Request) {
	n := randHex()
	h.mu.Lock()
	now := time.Now()
	for k, exp := range h.nonces { // прибираємо прострочене
		if now.After(exp) {
			delete(h.nonces, k)
		}
	}
	for k, exp := range h.sessions {
		if now.After(exp) {
			delete(h.sessions, k)
		}
	}
	if len(h.nonces) < 1000 {
		h.nonces[n] = now.Add(2 * time.Minute)
	}
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"nonce": n, "password": h.srv.password != "", "loggedIn": h.authorized(r), "name": h.srv.name,
	})
}

func (h *clipHub) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	if h.srv.blocked(ip) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var req struct{ Nonce, Proof string }
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	exp, known := h.nonces[req.Nonce]
	delete(h.nonces, req.Nonce) // число діє один раз
	h.mu.Unlock()
	sum := sha256.Sum256([]byte(req.Nonce + ":" + h.srv.password))
	ok := known && time.Now().Before(exp) && h.srv.password != "" &&
		subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(strings.ToLower(req.Proof))) == 1
	h.srv.authResult(ip, ok)
	if !ok {
		time.Sleep(time.Second)
		logf("%s: буфер обміну: невдалий вхід", ip)
		http.Error(w, "wrong password", http.StatusUnauthorized)
		return
	}
	token := randHex()
	h.mu.Lock()
	h.sessions[token] = time.Now().Add(12 * time.Hour)
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "svnc_clip", Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	logf("%s: буфер обміну: вхід", ip)
	w.WriteHeader(http.StatusNoContent)
}

// state — що зараз лежить у буфері обміну ПК.
func (h *clipHub) state(w http.ResponseWriter, r *http.Request) {
	seq, _, _ := pGetClipboardSequenceNumber.Call()
	text, _ := getClipboard()
	const maxText = 1 << 20
	truncated := len(text) > maxText
	if truncated {
		text = strings.ToValidUTF8(text[:maxText], "")
	}
	files := []clipFile{}
	for _, p := range getClipboardFiles() {
		if st, err := os.Stat(p); err == nil {
			files = append(files, clipFile{Name: filepath.Base(p), Size: st.Size(), Dir: st.IsDir()})
		} else {
			files = append(files, clipFile{Name: filepath.Base(p), Size: -1})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"seq": uint32(seq), "text": strings.ReplaceAll(text, "\r\n", "\n"), "truncated": truncated, "files": files,
	})
}

// macScript віддає сценарій для Mac з уже вписаною адресою ПК — тією, за якою
// Mac до нього звернувся. Секретів у сценарії немає, тож вхід не потрібен.
func (h *clipHub) macScript(w http.ResponseWriter, r *http.Request) {
	for _, c := range r.Host { // адреса потрапляє в сценарій — пропускаємо лише безпечні символи
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(".:-[]", c)) {
			http.Error(w, "bad host", http.StatusBadRequest)
			return
		}
	}
	script := strings.ReplaceAll(clipMacScript, "\r", "")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, strings.ReplaceAll(script, "__BASE__", "http://"+r.Host))
}

// plain — буфер обміну ПК простим текстом для сценарію: перший рядок — номер
// зміни, далі сам текст.
func (h *clipHub) plain(w http.ResponseWriter, r *http.Request) {
	seq, _, _ := pGetClipboardSequenceNumber.Call()
	text, _ := getClipboard()
	if len(text) > 1<<20 {
		text = ""
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%d\n%s", uint32(seq), strings.ReplaceAll(text, "\r\n", "\n"))
}

// text кладе надісланий текст у буфер обміну ПК.
func (h *clipHub) text(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil || len(body) == 0 {
		http.Error(w, "empty", http.StatusBadRequest)
		return
	}
	setClipboard(body)
	logf("%s: буфер обміну: отримано текст (%d байт)", clientIP(r), len(body))
	w.WriteHeader(http.StatusNoContent)
}

// safeName робить з імені файла безпечне для Windows.
func safeName(name string) string {
	name = strings.Map(func(c rune) rune {
		if c < 32 || strings.ContainsRune(`<>:"/\|?*`, c) {
			return '_'
		}
		return c
	}, filepath.Base(name))
	name = strings.TrimRight(name, " .")
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "LPT1", "LPT2", "LPT3":
		name = "_" + name
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// freePath добирає ім'я, якого ще немає: «звіт.pdf», «звіт (2).pdf»…
func freePath(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	for i := 2; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext))
	}
}

// upload зберігає надіслані файли й кладе їх у буфер обміну ПК.
func (h *clipHub) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "multipart expected", http.StatusBadRequest)
		return
	}
	dir := receivedDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, "cannot create folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var paths, names []string
	var total int64
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			continue
		}
		dst := freePath(dir, safeName(part.FileName()))
		f, err := os.Create(dst)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n, err := io.Copy(f, part) // потоком на диск: розмір файла не обмежений пам'яттю
		f.Close()
		if err != nil {
			os.Remove(dst)
			http.Error(w, "upload interrupted", http.StatusBadRequest)
			return
		}
		total += n
		paths, names = append(paths, dst), append(names, filepath.Base(dst))
	}
	if len(paths) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	onClipboard := setClipboardFiles(paths)
	logf("%s: буфер обміну: отримано файлів %d (%d байт) у %s", clientIP(r), len(paths), total, dir)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"saved": names, "dir": dir, "clipboard": onClipboard})
}

// file віддає файл номер i з буфера обміну ПК.
func (h *clipHub) file(w http.ResponseWriter, r *http.Request) {
	files := getClipboardFiles()
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil || i < 0 || i >= len(files) {
		http.Error(w, "this file is no longer on the clipboard", http.StatusNotFound)
		return
	}
	st, err := os.Stat(files[i])
	if err != nil || st.IsDir() {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}
	f, err := os.Open(files[i])
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	defer f.Close()
	name := filepath.Base(files[i])
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/octet-stream")
	logf("%s: буфер обміну: віддано файл (%d байт)", clientIP(r), st.Size())
	http.ServeContent(w, r, "", st.ModTime(), f)
}
