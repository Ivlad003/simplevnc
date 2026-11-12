//go:build windows

package main

import (
	"embed"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"golang.org/x/net/websocket"
)

// Веб-клієнт: вбудований noVNC (https://novnc.com, MPL 2.0) відкривається у
// звичайному браузері — на iPad чи телефоні не потрібен окремий застосунок.
// Браузер говорить той самий протокол RFB, лише всередині WebSocket.

//go:embed novnc
var novncFiles embed.FS

// wsConn підставляє справжню адресу клієнта: бібліотека повертає замість неї
// адресу сторінки, а нам потрібна IP для журналу й захисту від підбору пароля.
type wsConn struct {
	*websocket.Conn
	addr net.Addr
}

func (c wsConn) RemoteAddr() net.Addr { return c.addr }

// startWebClient піднімає веб-клієнт на всіх інтерфейсах; повертає порт.
func startWebClient(srv *Server, bind string) (int, error) {
	var ln net.Listener
	var err error
	port := 0
	for p := 6080; p < 6180; p++ { // 6080 — звичний порт noVNC
		if ln, err = net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(p))); err == nil {
			port = p
			break
		}
	}
	if err != nil {
		return 0, err
	}
	static, _ := fs.Sub(novncFiles, "novnc")
	mux := http.NewServeMux()
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" { // корінь одразу веде в клієнт з автопідключенням
			http.Redirect(w, r, webClientPath(""), http.StatusFound)
			return
		}
		files.ServeHTTP(w, r)
	})
	mux.Handle("/websockify", websocket.Server{
		// будь-яке джерело: доступ однаково захищає пароль VNC
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			for _, p := range cfg.Protocol {
				if p == "binary" {
					cfg.Protocol = []string{"binary"}
					return nil
				}
			}
			cfg.Protocol = nil
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			ws.PayloadType = websocket.BinaryFrame
			addr, err := net.ResolveTCPAddr("tcp", ws.Request().RemoteAddr)
			if err != nil {
				addr = &net.TCPAddr{}
			}
			srv.serve(wsConn{ws, addr})
		},
	})
	go http.Serve(ln, mux)
	return port, nil
}

// webClientPath — шлях до веб-клієнта з автопідключенням і підгонкою під екран.
// Пароль іде у фрагменті (#…): браузер не надсилає його серверу й не пише в журнали.
func webClientPath(password string) string {
	p := "/vnc.html?autoconnect=1&resize=scale&reconnect=1&reconnect_delay=2000"
	if password != "" {
		p += "#password=" + url.QueryEscape(password)
	}
	return p
}
