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
	IP    string
	Label string
	URL   template.URL // vnc://ip:port; довірений тип, інакше шаблон вирізає схему vnc
	Host  string       // ip:port
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
}

// startWeb піднімає сторінку підключення лише на 127.0.0.1: її бачить той, хто
// сидить за цим ПК, а пароль не потрапляє в мережу.
func startWeb(srv *Server, port int, bind string) (*webUI, error) {
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
	u := &webUI{srv: srv, port: port, bind: bind, httpAddr: ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", u.guard(u.page))
	mux.HandleFunc("/qr.png", u.guard(u.qr))
	mux.HandleFunc("/api/status", u.guard(u.status))
	mux.HandleFunc("/api/scale", u.guard(u.setScale))
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pageTmpl.Execute(w, map[string]any{
		"Name":     u.srv.name,
		"Addrs":    listAddrs(u.port, u.bind),
		"Password": u.srv.password,
		"Port":     u.port,
		"Steps":    scaleSteps,
	})
}

// qr віддає QR-код адреси з номером i.
func (u *webUI) qr(w http.ResponseWriter, r *http.Request) {
	addrs := listAddrs(u.port, u.bind)
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil || i < 0 || i >= len(addrs) {
		http.NotFound(w, r)
		return
	}
	code, err := qr.Encode(string(addrs[i].URL), qr.M)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	code.Scale = 8
	w.Header().Set("Content-Type", "image/png")
	w.Write(code.PNG())
}

func (u *webUI) status(w http.ResponseWriter, r *http.Request) {
	s := u.srv.scr
	s.mu.RLock()
	clients := make([]string, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c.addr)
	}
	s.mu.RUnlock()
	sort.Strings(clients)
	_, _, sw, sh := virtualScreen()
	sc := int(s.scale.Load())
	if sc <= 0 || sc >= 100 {
		sc = 100
	}
	fw, fh := s.frameSize()
	frame := fmt.Sprintf("%d×%d", fw, fh)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"clients": clients,
		"scale":   sc,
		"screen":  fmt.Sprintf("%d×%d", sw, sh),
		"frame":   frame,
	})
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
	w.WriteHeader(http.StatusNoContent)
}
