//go:build windows

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

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

// asset — файл веб-клієнта, готовий до віддачі: стиснений заздалегідь і з міткою
// вмісту, щоб браузер не завантажував його вдруге.
type asset struct {
	raw, gz []byte
	ctype   string
	etag    string
}

// Типи задаємо самі: системна таблиця Windows інколи називає .js «текстом», і
// тоді браузер відмовляється виконувати модулі.
var assetTypes = map[string]string{
	".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".html": "text/html; charset=utf-8",
	".json": "application/json", ".svg": "image/svg+xml", ".txt": "text/plain; charset=utf-8",
	".png": "image/png", ".ico": "image/x-icon", ".mp3": "audio/mpeg", ".oga": "audio/ogg",
	".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf",
}

func loadAssets() map[string]*asset {
	out := map[string]*asset{}
	fs.WalkDir(novncFiles, "novnc", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := novncFiles.ReadFile(p)
		if err != nil {
			return nil
		}
		ext := strings.ToLower(path.Ext(p))
		sum := sha256.Sum256(b)
		a := &asset{raw: b, ctype: assetTypes[ext], etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		if a.ctype == "" {
			a.ctype = "application/octet-stream"
		}
		switch ext { // текстові формати стискаються в кілька разів
		case ".js", ".mjs", ".css", ".html", ".json", ".svg", ".txt", ".ico":
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if buf.Len() < len(b)*9/10 {
				a.gz = buf.Bytes()
			}
		}
		out[strings.TrimPrefix(p, "novnc")] = a
		return nil
	})
	return out
}

func (a *asset) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	h.Set("Vary", "Accept-Encoding")
	if strings.HasPrefix(a.ctype, "text/html") {
		h.Set("Cache-Control", "no-cache") // сторінку щоразу звіряємо за міткою
	} else {
		h.Set("Cache-Control", "public, max-age=86400")
	}
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.raw
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// startPage — сторінка-пускач: добирає розмір кадру під вікно браузера й одразу
// переходить у веб-клієнт. Менший кадр — менше даних і менше роботи для браузера.
// Варіант ?q=sharp враховує щільність екрана (чіткіше, але повільніше), ?scale=N
// задає масштаб явно. Частину адреси після # (пароль) зберігаємо.
const startPage = `<!doctype html>
<html lang="uk"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SimpleVNC</title></head>
<body style="background:#0f1115;color:#9aa1ad;font:16px system-ui,sans-serif;margin:24px">
Підключення…
<script>
(function () {
  var q = new URLSearchParams(location.search), screenWidth = %d, steps = [25, 33, 50, 66, 75, 100];
  var scale = 100, asked = parseInt(q.get('scale'), 10);
  if (steps.indexOf(asked) >= 0) {
    scale = asked;
  } else {
    var px = window.innerWidth * (q.get('q') === 'sharp' ? (window.devicePixelRatio || 1) : 1);
    for (var i = 0; i < steps.length; i++) {
      if (steps[i] >= px / screenWidth * 100) { scale = steps[i]; break; }
    }
  }
  location.replace('vnc.html?autoconnect=1&resize=scale&reconnect=1&reconnect_delay=2000&path=' +
    encodeURIComponent('websockify?scale=' + scale) + location.hash);
})();
</script>
</body></html>`

// startWebClient піднімає веб-клієнт на всіх інтерфейсах; повертає порт, а також
// маршрутизатор і вузол входу — щоб додати на той самий порт інші сторінки.
func startWebClient(srv *Server, bind string) (int, *http.ServeMux, *clipHub, error) {
	var ln net.Listener
	var err error
	port := 0
	for p := 6080; p < 6180; p++ { // 6080 — звичний порт noVNC
		if ln, err = listenTCP(bind, p); err == nil {
			port = p
			break
		}
	}
	if err != nil {
		return 0, nil, nil, err
	}
	assets := loadAssets()
	mux := http.NewServeMux()
	hub := newClipHub(srv, mux) // спільний буфер обміну: /clip/
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _, aw, _ := captureArea()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprintf(w, startPage, aw)
			return
		}
		if a := assets[path.Clean(r.URL.Path)]; a != nil {
			a.serve(w, r)
			return
		}
		http.NotFound(w, r)
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
			// ?scale=N — розмір кадру саме для цього клієнта
			scale, _ := strconv.Atoi(ws.Request().URL.Query().Get("scale"))
			if !validScale(scale) {
				scale = 0
			}
			srv.serveScaled(wsConn{ws, addr}, scale)
		},
	})
	go http.Serve(ln, mux)
	return port, mux, hub, nil
}

// webClientPath — шлях до веб-клієнта. sharp — чіткіша картинка коштом швидкості.
// Пароль іде у фрагменті (#…): браузер не надсилає його серверу й не пише в журнали.
func webClientPath(password string, sharp bool) string {
	p := "/"
	if sharp {
		p += "?q=sharp"
	}
	if password != "" {
		p += "#password=" + url.QueryEscape(password)
	}
	return p
}
