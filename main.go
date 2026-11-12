//go:build windows

// SimpleVNC — мінімальний VNC-сервер для Windows: запустив і підключайся.
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

var verbose, noDXGI bool

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
	noOpen   bool
	bonjour  bool
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
	flag.BoolVar(&forceDrawnCurs, "drawcursor", false, "завжди малювати курсор у кадрі (видно рухи миші за самим ПК)")
	noBonjour := flag.Bool("nobonjour", false, "не оголошувати ПК у локальній мережі (Finder на Mac)")
	install := flag.Bool("install", false, "встановити як службу Windows: автозапуск, вікна UAC та екран входу")
	uninstall := flag.Bool("uninstall", false, "видалити службу Windows")
	service := flag.Bool("service", false, "(внутрішнє) запуск диспетчером служб")
	worker := flag.Bool("worker", false, "(внутрішнє) робочий процес служби")
	elevated := flag.Bool("elevated", false, "(внутрішнє) перезапуск через UAC")
	flag.Parse()

	pSetConsoleOutputCP.Call(65001)

	explicitPort := false
	flag.Visit(func(f *flag.Flag) { explicitPort = explicitPort || f.Name == "port" })

	password := *pass
	if len(password) > 8 {
		password = password[:8] // VNC-автентифікація використовує лише 8 символів
	}

	switch {
	case *service:
		serviceMode = true
		runService()
		return
	case *install || *uninstall:
		c := svcConfig{Password: password, Scale: *scale, Area: *area}
		if explicitPort {
			c.Port = *port
		}
		os.Exit(serviceCommand(*install, c, *nopass, *elevated))
	}

	o := options{port: *port, portAuto: !explicitPort, bind: *bind, password: password,
		scale: *scale, area: *area, noOpen: *noOpen, bonjour: !*noBonjour}
	if *worker {
		serviceMode = true
		setupFileLog()
		c, err := loadConfig()
		if err != nil {
			logf("не вдалося прочитати налаштування служби: %v", err)
			os.Exit(1)
		}
		o = options{port: c.Port, portAuto: true, password: c.Password, scale: c.Scale, area: c.Area,
			noOpen: true, bonjour: true}
	} else {
		switch {
		case *nopass:
			o.password = ""
		case o.password == "":
			o.password = randomPassword()
		}
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
		if ln, err = net.Listen("tcp", net.JoinHostPort(o.bind, fmt.Sprint(p))); err == nil {
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
	srv := &Server{scr: scr, password: o.password, name: host, fails: map[string]*failRec{}}
	go inputLoop()
	go scr.run()
	go scr.watchClipboard()
	// оголошення має сенс лише коли сервер досяжний із мережі
	loopback := net.ParseIP(o.bind) != nil && net.ParseIP(o.bind).IsLoopback()
	if o.bonjour && (!loopback || os.Getenv("SIMPLEVNC_FORCE_BONJOUR") != "") {
		go advertiseBonjour(host, o.port)
	}

	if serviceMode {
		logf("робочий процес служби: порт %d, кадр %dx%d", o.port, scr.w, scr.h)
	} else {
		fmt.Println("SimpleVNC запущено")
		fmt.Printf("  Екран:   %dx%d", scr.w, scr.h)
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
	}
	webPort, err := startWebClient(srv, o.bind)
	if err != nil {
		logf("веб-клієнт не вдалося запустити: %v", err)
	} else if !serviceMode {
		fmt.Printf("  У браузері: http://адреса:%d — без VNC-клієнта (iPad, телефон)\n", webPort)
	} else {
		logf("веб-клієнт: порт %d", webPort)
	}
	if web, err := startWeb(srv, o.port, o.bind, webPort); err != nil {
		logf("сторінку підключення не вдалося запустити: %v", err)
	} else if !serviceMode {
		fmt.Printf("  Сторінка з QR-кодами: %s\n", web.url())
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
