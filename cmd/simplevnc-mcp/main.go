// simplevnc-mcp — міст між ШІ-агентом і віддаленим ПК.
//
// Для агента (Claude Code, Cursor) це сервер MCP з інструментами «знімок екрана»,
// «клік», «ввести текст» тощо. До ПК він підключається як звичайний VNC-клієнт,
// тож працює з будь-якого комп'ютера: Mac, Linux чи Windows.
//
//	SIMPLEVNC_HOST=192.168.1.20:5912 SIMPLEVNC_PASSWORD=… simplevnc-mcp
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

// logf пише в stderr: stdout зайнятий протоколом MCP.
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func main() {
	host := flag.String("host", os.Getenv("SIMPLEVNC_HOST"), "адреса VNC-сервера, наприклад 192.168.1.20:5912 (або змінна SIMPLEVNC_HOST)")
	password := flag.String("password", "", "пароль VNC; краще задавати змінною SIMPLEVNC_PASSWORD")
	viewOnly := flag.Bool("view-only", os.Getenv("SIMPLEVNC_VIEW_ONLY") != "", "лише дивитися: без миші та клавіатури")
	flag.Parse()

	if *password == "" {
		*password = os.Getenv("SIMPLEVNC_PASSWORD")
	}
	if *host == "" {
		fmt.Fprintln(os.Stderr, "Не задано адресу VNC-сервера: параметр -host або змінна SIMPLEVNC_HOST.")
		os.Exit(2)
	}
	if _, _, err := net.SplitHostPort(*host); err != nil {
		*host = net.JoinHostPort(*host, "5900") // порт не вказано — стандартний
	}

	srv := &mcpServer{vnc: newVNC(*host, *password), viewOnly: *viewOnly}
	logf("simplevnc-mcp: сервер %s, режим %s", *host, map[bool]string{true: "лише перегляд", false: "повне керування"}[*viewOnly])
	if err := srv.serve(os.Stdin, os.Stdout); err != nil {
		logf("simplevnc-mcp: %v", err)
		os.Exit(1)
	}
}
