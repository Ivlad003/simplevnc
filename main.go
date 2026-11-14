//go:build windows

// SimpleVNC — мінімальний VNC-сервер для Windows: запустив і підключайся.
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var verbose, noDXGI bool

// version задається під час збірки: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func logf(format string, a ...any) {
	stamp := "15:04:05"
	if serviceMode {
		stamp = "2006-01-02 15:04:05" // журнал служби живе довше за добу
	}
	fmt.Fprintf(os.Stdout, "%s  %s\n", time.Now().Format(stamp), fmt.Sprintf(format, a...))
}

// randomPassword — 8 символів без схожих між собою знаків.
func randomPassword() string {
	const abc = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b)
}

// options — усе, що потрібно серверу для запуску.
type options struct {
	port     int
	portAuto bool // якщо порт зайнятий, узяти наступний вільний
	bind     string
	password string
	scale    int
	area     string
	mode     string // роздільність екрана ПК на час підключення, «1920x1200»
	noOpen   bool
	bonjour  bool
	agentPw  string
	agentRO  bool
}

func main() {
	port := flag.Int("port", 5900, "TCP-порт")
	bind := flag.String("bind", "", "адреса інтерфейсу (типово — усі)")
	pass := flag.String("p", "", "постійний пароль (до 8 символів); без нього — випадковий на кожен запуск")
	nopass := flag.Bool("nopass", false, "без пароля (лише для довіреної мережі)")
	flag.BoolVar(&verbose, "v", false, "показувати статистику захоплення")
	flag.BoolVar(&noDXGI, "gdi", false, "захоплювати лише через GDI (без DXGI)")
	scale := flag.Int("scale", 100, "розмір кадру у відсотках: 100, 75, 66, 50, 33 або 25")
	noOpen := flag.Bool("noopen", false, "не відкривати сторінку підключення в браузері")
	area := flag.String("area", "all", "що транслювати: all, left, right, m1, m2… або x,y,ширина,висота")
	mode := flag.String("mode", "", "роздільність екрана ПК, поки хтось підключений, наприклад 1920x1200; після відключення повертається звичайна")
	flag.BoolVar(&forceDrawnCurs, "drawcursor", false, "завжди малювати курсор у кадрі (видно рухи миші за самим ПК)")
	agentPass := flag.String("agentpass", "", "окремий пароль для ШІ-агентів (до 8 символів): без доступу до вікон дозволу Windows")
	agentView := flag.Bool("agentview", false, "агенти з паролем -agentpass лише дивляться, без миші та клавіатури")
	flag.StringVar(&clipDirOverride, "clipdir", "", "куди зберігати файли, надіслані через спільний буфер обміну (типово — Завантаження/SimpleVNC)")
	noBonjour := flag.Bool("nobonjour", false, "не оголошувати ПК у локальній мережі (Finder на Mac)")
	install := flag.Bool("install", false, "встановити як службу Windows: автозапуск, вікна UAC та екран входу")
	uninstall := flag.Bool("uninstall", false, "видалити службу Windows")
	service := flag.Bool("service", false, "(внутрішнє) запуск диспетчером служб")
	worker := flag.Bool("worker", false, "(внутрішнє) робочий процес служби")
	elevated := flag.Bool("elevated", false, "(внутрішнє) перезапуск через UAC")
	showVersion := flag.Bool("version", false, "показати версію програми")
	flag.Parse()

	pSetConsoleOutputCP.Call(65001)
	if *showVersion {
		fmt.Println("SimpleVNC", version)
		return
	}

	given := map[string]bool{} // параметри, задані явно
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })
	explicitPort := given["port"]

	restarted, inheritedPass := afterRestart()
	if !*service && !*worker && !*install && !*uninstall { // звичайний запуск: беремо збережене зі сторінки
		saved := loadSettings()
		if !given["scale"] && validScale(saved.Scale) {
			*scale = saved.Scale
		}
		if !given["area"] && saved.Area != "" {
			*area = saved.Area
		}
		if !given["mode"] && saved.Mode != "" {
			*mode = saved.Mode
		}
	}

	password := *pass
	if len(password) > 8 {
		password = password[:8] // VNC-автентифікація використовує лише 8 символів
	}

	if len(*agentPass) > 8 {
		*agentPass = (*agentPass)[:8]
	}
	if *agentPass != "" && *agentPass == password {
		fmt.Println("Пароль для агентів збігається з основним — обмеження для агентів не діятимуть. Задайте інший.")
		os.Exit(2)
	}

	switch {
	case *service:
		serviceMode = true
		runService()
		return
	case *install || *uninstall:
		c := svcConfig{Password: password, Scale: *scale, Area: *area, Mode: *mode, AgentPassword: *agentPass, AgentView: *agentView}
		if explicitPort {
			c.Port = *port
		}
		os.Exit(serviceCommand(*install, c, *nopass, *elevated, given))
	}

	o := options{port: *port, portAuto: !explicitPort, bind: *bind, password: password,
		scale: *scale, area: *area, mode: *mode, noOpen: *noOpen, bonjour: !*noBonjour, agentPw: *agentPass, agentRO: *agentView}
	if *worker {
		serviceMode = true
		setupFileLog()
		c, err := loadConfig()
		if err != nil {
			logf("не вдалося прочитати налаштування служби: %v", err)
			os.Exit(1)
		}
		o = options{port: c.Port, portAuto: true, password: c.Password, scale: c.Scale, area: c.Area, mode: c.Mode,
			noOpen: true, bonjour: true, agentPw: c.AgentPassword, agentRO: c.AgentView}
	} else {
		switch {
		case *nopass:
			o.password = ""
		case o.password == "" && restarted && inheritedPass != "":
			o.password = inheritedPass // перезапуск зі сторінки: пароль лишається тим самим
		case o.password == "":
			o.password = randomPassword()
		}
		o.noOpen = o.noOpen || restarted // сторінка вже відкрита
	}
	runServer(o)
}

func runServer(o options) {
	setDPIAware()

	// Порт не задано явно — беремо перший вільний, починаючи з 5900
	// (Windows інколи резервує 5900 під Hyper-V/WSL).
	last := o.port
	if o.portAuto {
		last = o.port + 99
	}
	var ln net.Listener
	var err error
	for p := o.port; p <= last; p++ {
		if ln, err = listenTCP(o.bind, p); err == nil {
			o.port = p
			break
		}
	}
	if err != nil {
		logf("не вдалося відкрити порт: %v", err)
		os.Exit(1)
	}

	if sel, err := parseArea(o.area); err != nil {
		logf("%v — транслюю весь екран", err)
	} else {
		curArea.Store(sel)
	}
	host, _ := os.Hostname()
	scr := newScreen()
	if !scr.setScale(o.scale) {
		logf("невідомий масштаб, лишаю 100%%; дозволені: %v", scaleSteps)
	}
	srv := &Server{scr: scr, password: o.password, name: host, fails: map[string]*failRec{},
		agentPassword: o.agentPw, agentView: o.agentRO}
	if o.password == "" {
		srv.agentPassword = "" // без основного пароля окремий пароль агента не має сенсу
	}
	srv.disp = newDisplayCtl(scr, o.mode)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM) // Ctrl+C або закрили вікно
	go func() {
		<-stop
		releaseAllInput()
		srv.disp.shutdown() // повертаємо екрану звичайну роздільність
		os.Exit(0)
	}()
	go inputLoop()
	go scr.run()
	go scr.watchClipboard()
	// оголошення має сенс лише коли сервер досяжний із мережі
	loopback := net.ParseIP(o.bind) != nil && net.ParseIP(o.bind).IsLoopback()
	if o.bonjour && (!loopback || os.Getenv("SIMPLEVNC_FORCE_BONJOUR") != "") {
		go advertiseBonjour(host, o.port)
	}

	_, _, areaW, areaH := captureArea()
	if serviceMode {
		logf("робочий процес служби %s: порт %d, кадр %dx%d", version, o.port, areaW, areaH)
	} else {
		fmt.Println("SimpleVNC", version, "запущено")
		fmt.Printf("  Екран:   %dx%d", areaW, areaH)
		if sc := int(scr.scale.Load()); sc != 100 {
			fmt.Printf(" (трансляція %d%%)", sc)
		}
		fmt.Println()
		for _, a := range listAddrs(o.port, o.bind) {
			fmt.Printf("  Адреса:  %-22s %s\n", a.Host, a.Label)
		}
		if o.password == "" {
			fmt.Println("  Пароль:  немає (будь-хто в мережі може підключитися!)")
		} else {
			fmt.Printf("  Пароль:  %s\n", o.password)
		}
		if srv.agentPassword != "" {
			mode := "керування без вікон дозволу Windows"
			if srv.agentView {
				mode = "лише перегляд"
			}
			fmt.Printf("  Пароль для агентів: %s (%s)\n", srv.agentPassword, mode)
		}
	}
	webPort, webMux, hub, err := startWebClient(srv, o.bind)
	if err != nil {
		logf("веб-клієнт не вдалося запустити: %v", err)
	} else if !serviceMode {
		fmt.Printf("  У браузері: http://адреса:%d — без VNC-клієнта (iPad, телефон)\n", webPort)
	} else {
		logf("веб-клієнт: порт %d", webPort)
	}
	web, err := startWeb(srv, o.port, o.bind, webPort)
	if err == nil && webMux != nil {
		web.mountRemote(webMux, hub) // та сама сторінка для інших пристроїв, за паролем
	}
	if err != nil {
		logf("сторінку підключення не вдалося запустити: %v", err)
	} else if !serviceMode {
		fmt.Printf("  Сторінка з QR-кодами: %s\n", web.url())
		if webMux != nil {
			fmt.Printf("  Вона ж з інших пристроїв: http://адреса:%d/panel/ (вхід паролем)\n", webPort)
		}
		if !o.noOpen {
			web.openBrowser()
		}
	} else {
		logf("сторінка підключення: %s", web.url())
	}
	if !serviceMode {
		fmt.Println("Закрийте це вікно, щоб зупинити сервер.")
		fmt.Println()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
		}
		go srv.serve(conn)
	}
}
