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
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
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

func main() {
	port := flag.Int("port", 5900, "TCP-порт")
	bind := flag.String("bind", "", "адреса інтерфейсу (типово — усі)")
	pass := flag.String("p", "", "постійний пароль (до 8 символів); без нього — випадковий на кожен запуск")
	nopass := flag.Bool("nopass", false, "без пароля (лише для довіреної мережі)")
	flag.BoolVar(&verbose, "v", false, "показувати статистику захоплення")
	flag.BoolVar(&noDXGI, "gdi", false, "захоплювати лише через GDI (без DXGI)")
	scale := flag.Int("scale", 100, "розмір кадру у відсотках: 100, 75, 66, 50, 33 або 25")
	noOpen := flag.Bool("noopen", false, "не відкривати сторінку підключення в браузері")
	flag.Parse()

	pSetConsoleOutputCP.Call(65001)
	setDPIAware()

	password := *pass
	switch {
	case *nopass:
		password = ""
	case password == "":
		password = randomPassword()
	case len(password) > 8:
		password = password[:8] // VNC-автентифікація використовує лише 8 символів
	}

	// Порт не задано явно — беремо перший вільний, починаючи з 5900
	// (Windows інколи резервує 5900 під Hyper-V/WSL).
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "port" })
	last := *port
	if !explicit {
		last = *port + 99
	}
	var ln net.Listener
	var err error
	for p := *port; p <= last; p++ {
		if ln, err = net.Listen("tcp", net.JoinHostPort(*bind, fmt.Sprint(p))); err == nil {
			*port = p
			break
		}
	}
	if err != nil {
		fmt.Println("Не вдалося відкрити порт:", err)
		os.Exit(1)
	}

	host, _ := os.Hostname()
	scr := newScreen()
	if !scr.setScale(*scale) {
		fmt.Println("Невідомий масштаб, лишаю 100%. Дозволені:", scaleSteps)
	}
	srv := &Server{scr: scr, password: password, name: host, fails: map[string]*failRec{}}
	go scr.run()
	go scr.watchClipboard()

	fmt.Println("SimpleVNC запущено")
	fmt.Printf("  Екран:   %dx%d", scr.w, scr.h)
	if sc := int(scr.scale.Load()); sc != 100 {
		fmt.Printf(" (трансляція %d%%)", sc)
	}
	fmt.Println()
	for _, a := range listAddrs(*port, *bind) {
		fmt.Printf("  Адреса:  %-22s %s\n", a.Host, a.Label)
	}
	if password == "" {
		fmt.Println("  Пароль:  немає (будь-хто в мережі може підключитися!)")
	} else {
		fmt.Printf("  Пароль:  %s\n", password)
	}
	if web, err := startWeb(srv, *port, *bind); err != nil {
		fmt.Println("  Сторінку підключення не вдалося запустити:", err)
	} else {
		fmt.Printf("  Сторінка з QR-кодами: %s\n", web.url())
		if !*noOpen {
			web.openBrowser()
		}
	}
	fmt.Println("Закрийте це вікно, щоб зупинити сервер.")
	fmt.Println()

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
