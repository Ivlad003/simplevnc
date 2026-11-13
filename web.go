//go:build windows

package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"rsc.io/qr"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// addrInfo — адреса, за якою сервер видно ззовні.
type addrInfo struct {
	IP       string
	Label    string
	URL      template.URL // vnc://ip:port; довірений тип, інакше шаблон вирізає схему vnc
	Host     string       // ip:port
	WebURL   template.URL // веб-клієнт у браузері; порожньо, якщо його не запущено
	WebSharp template.URL // він же з чіткішою картинкою (повільніше)
	ClipURL  template.URL // сторінка спільного буфера обміну
	PanelURL template.URL // ця сама сторінка для інших пристроїв (після входу паролем)
}

// virtualAdapter — мережеві адаптери, недосяжні з інших пристроїв.
func virtualAdapter(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"vethernet", "wsl", "hyper-v", "virtualbox", "vmware", "loopback", "bluetooth", "docker"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// listAddrs повертає адреси для підключення: спершу локальна мережа, далі VPN.
func listAddrs(port int, bind string) []addrInfo {
	var out []addrInfo
	add := func(ip net.IP, label string) {
		h := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		out = append(out, addrInfo{IP: ip.String(), Label: label, URL: template.URL("vnc://" + h), Host: h})
	}
	if bind != "" {
		if ip := net.ParseIP(bind); ip != nil {
			add(ip, "Задана адреса")
		}
		return out
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10") // Tailscale та подібні VPN
	ifs, _ := net.Interfaces()
	for _, it := range ifs {
		if it.Flags&net.FlagUp == 0 || it.Flags&net.FlagLoopback != 0 || virtualAdapter(it.Name) {
			continue
		}
		addrs, _ := it.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || n.IP.IsLinkLocalUnicast() {
				continue
			}
			if cgnat.Contains(n.IP) {
				add(n.IP.To4(), "Tailscale / VPN — працює з будь-якої мережі")
			} else {
				add(n.IP.To4(), "Локальна мережа — "+it.Name)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return !strings.HasPrefix(out[i].Label, "Tailscale") && strings.HasPrefix(out[j].Label, "Tailscale")
	})
	return out
}

type webUI struct {
	srv      *Server
	port     int
	bind     string
	httpAddr string
	webPort  int // порт веб-клієнта; 0 — не запущено
}

// addrs — адреси з посиланнями на веб-клієнт. На сторінці, відкритій на самому ПК
// у звичайному режимі, пароль входить у посилання (її бачить лише той, хто за цим
// ПК). У режимі служби та на мережевій копії сторінки (remote) — ні.
func (u *webUI) addrs(remote bool) []addrInfo {
	list := listAddrs(u.port, u.bind)
	if u.webPort == 0 {
		return list
	}
	pw := u.srv.password
	if serviceMode || remote {
		pw = ""
	}
	for i := range list {
		base := "http://" + net.JoinHostPort(list[i].IP, strconv.Itoa(u.webPort))
		list[i].WebURL = template.URL(base + webClientPath(pw, false))
		list[i].WebSharp = template.URL(base + webClientPath(pw, true))
		list[i].ClipURL = template.URL(base + "/clip/")
		list[i].PanelURL = template.URL(base + "/panel/")
	}
	return list
}

// mountRemote додає мережеву копію сторінки підключення: /panel/ на порту
// веб-клієнта. Вона відкривається з інших пристроїв (наприклад, через Tailscale)
// лише після входу паролем і сам пароль не показує.
func (u *webUI) mountRemote(mux *http.ServeMux, hub *clipHub) {
	mux.HandleFunc("/panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusFound)
	})
	mux.HandleFunc("/panel/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/panel/" {
			http.NotFound(w, r)
			return
		}
		if !hub.authorized(r) {
			http.Redirect(w, r, "/clip/?next=/panel/", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		u.render(w, true)
	})
	mux.HandleFunc("/panel/qr.png", hub.guard(u.qrHandler(true)))
	mux.HandleFunc("/panel/api/status", hub.guard(u.status))
	mux.HandleFunc("/panel/api/scale", hub.guard(u.setScale))
	mux.HandleFunc("/panel/api/area", hub.guard(u.setArea))
	mux.HandleFunc("/panel/api/display", hub.guard(u.setDisplay))
	mux.HandleFunc("/panel/api/restart", hub.guard(u.restart))
}

// startWeb піднімає сторінку підключення лише на 127.0.0.1: її бачить той, хто
// сидить за цим ПК, а пароль не потрапляє в мережу.
func startWeb(srv *Server, port int, bind string, webPort int) (*webUI, error) {
	var ln net.Listener
	var err error
	for p := 5980; p < 6080; p++ {
		if ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	u := &webUI{srv: srv, port: port, bind: bind, httpAddr: ln.Addr().String(), webPort: webPort}
	mux := http.NewServeMux()
	mux.HandleFunc("/", u.guard(u.page))
	mux.HandleFunc("/qr.png", u.guard(u.qrHandler(false)))
	mux.HandleFunc("/api/status", u.guard(u.status))
	mux.HandleFunc("/api/scale", u.guard(u.setScale))
	mux.HandleFunc("/api/area", u.guard(u.setArea))
	mux.HandleFunc("/api/display", u.guard(u.setDisplay))
	mux.HandleFunc("/api/restart", u.guard(u.restart))
	go http.Serve(ln, mux)
	return u, nil
}

func (u *webUI) url() string { return "http://" + u.httpAddr + "/" }

func (u *webUI) openBrowser() {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", u.url()).Start()
}

// guard пропускає лише запити, адресовані саме цій локальній сторінці: це закриває
// DNS rebinding і запити з чужих сайтів, відкритих у тому ж браузері.
func (u *webUI) guard(h http.HandlerFunc) http.HandlerFunc {
	_, port, _ := net.SplitHostPort(u.httpAddr)
	okHosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	return func(w http.ResponseWriter, r *http.Request) {
		if !okHosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !okHosts[strings.TrimPrefix(o, "http://")] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func (u *webUI) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	u.render(w, false)
}

// render малює сторінку: remote — мережева копія, без пароля.
func (u *webUI) render(w http.ResponseWriter, remote bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pageTmpl.Execute(w, map[string]any{
		"Name":     u.srv.name,
		"Addrs":    u.addrs(remote),
		"Password": u.srv.password,
		"Service":  serviceMode,
		"Remote":   remote,
		"Port":     u.port,
		"Web":      u.webPort != 0,
		"Steps":    scaleSteps,
		"Version":  version,
	})
}

// qrHandler віддає QR-код адреси з номером i: k=web — посилання на веб-клієнт,
// інакше vnc://.
func (u *webUI) qrHandler(remote bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { u.qr(w, r, remote) }
}

func (u *webUI) qr(w http.ResponseWriter, r *http.Request, remote bool) {
	addrs := u.addrs(remote)
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil || i < 0 || i >= len(addrs) {
		http.NotFound(w, r)
		return
	}
	text := string(addrs[i].URL)
	if r.URL.Query().Get("k") == "web" {
		if text = string(addrs[i].WebURL); text == "" {
			http.NotFound(w, r)
			return
		}
	}
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	code.Scale = 6
	w.Header().Set("Content-Type", "image/png")
	w.Write(code.PNG())
}

func (u *webUI) status(w http.ResponseWriter, r *http.Request) {
	s := u.srv.scr
	clients := []string{}
	for _, c := range s.allClients() {
		clients = append(clients, c.addr)
	}
	sort.Strings(clients)
	_, _, aw, ah := captureArea()
	sc := int(s.scale.Load())
	if sc <= 0 || sc >= 100 {
		sc = 100
	}
	fw, fh := s.frameSize()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"clients": clients,
		"scale":   sc,
		"screen":  fmt.Sprintf("%d×%d", aw, ah),
		"frame":   fmt.Sprintf("%d×%d", fw, fh),
		"area":    areaID(),
		"areas":   areaChoices(),
		"display": u.srv.disp.status(),
		"boot":    bootID,
		"version": version,
	})
}

// setDisplay обирає роздільність екрана ПК на час підключення; v=native — не міняти.
func (u *webUI) setDisplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("v")
	if err := u.srv.disp.set(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	logf("роздільність екрана на час підключення: %s", id)
	u.srv.saveSettings()
	w.WriteHeader(http.StatusNoContent)
}

func (u *webUI) setScale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	v, _ := strconv.Atoi(r.URL.Query().Get("v"))
	if !u.srv.scr.setScale(v) {
		http.Error(w, "bad scale", http.StatusBadRequest)
		return
	}
	logf("масштаб кадру змінено на %d%%", v)
	u.srv.saveSettings()
	w.WriteHeader(http.StatusNoContent)
}

func (u *webUI) setArea(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("v")
	if err := u.srv.scr.setArea(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	logf("область трансляції змінено на %s", id)
	u.srv.saveSettings()
	w.WriteHeader(http.StatusNoContent)
}
