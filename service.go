//go:build windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Режим служби. Служба (SYSTEM, сеанс 0) сама екран не бачить, тому запускає
// робочий процес у сеансі, де зараз консоль користувача. Робочий процес має права
// SYSTEM, тож бачить вікна дозволу UAC та екран входу й може ними керувати.

const svcName = "SimpleVNC"

// serviceMode — цей процес є робочим процесом служби: без консолі й браузера,
// журнал у файлі, пароль на сторінці підключення не показується.
var serviceMode bool

type svcConfig struct {
	Password string `json:"password"`
	Port     int    `json:"port"`
	Scale    int    `json:"scale"`
	Area     string `json:"area"`
	Mode     string `json:"mode,omitempty"` // роздільність екрана на час підключення

	AgentPassword string `json:"agentPassword,omitempty"`
	AgentView     bool   `json:"agentView,omitempty"`
}

func dataDir() string {
	d := os.Getenv("ProgramData")
	if d == "" {
		d = `C:\ProgramData`
	}
	return filepath.Join(d, "SimpleVNC")
}
func configPath() string { return filepath.Join(dataDir(), "service.json") }
func logPath() string    { return filepath.Join(dataDir(), "simplevnc.log") }

func installDir() string {
	d := os.Getenv("ProgramFiles")
	if d == "" {
		d = `C:\Program Files`
	}
	return filepath.Join(d, "SimpleVNC")
}

// saveConfig пише налаштування служби у файл, доступний лише SYSTEM та
// адміністраторам: там лежить пароль, а доступ до VNC — це доступ до консолі.
//
// Пишемо в сусідній файл і підміняємо ним старий: робочий процес служби оновлює
// налаштування на ходу, і збій посеред запису не має лишити службу без них.
func saveConfig(c svcConfig) error {
	if err := os.MkdirAll(dataDir(), 0755); err != nil {
		return err
	}
	tmp := configPath() + ".tmp"
	os.Remove(tmp) // права задаються лише під час створення файла
	sd, err := windows.SecurityDescriptorFromString(configSDDL)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, _ := windows.UTF16PtrFromString(tmp)
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), tmp)
	err = json.NewEncoder(f).Encode(c)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, configPath())
}

// configSDDL — права на файл налаштувань: лише SYSTEM та адміністратори.
var configSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

func loadConfig() (c svcConfig, err error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// setupFileLog спрямовує журнал у файл (у служби немає консолі).
func setupFileLog() {
	os.MkdirAll(dataDir(), 0755)
	if st, err := os.Stat(logPath()); err == nil && st.Size() > 1<<20 {
		os.Rename(logPath(), logPath()+".old")
	}
	if f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		os.Stdout, os.Stderr = f, f
	}
}

func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// relaunchElevated перезапускає програму з тими самими параметрами через UAC.
func relaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := make([]string, 0, len(os.Args))
	for _, a := range os.Args[1:] {
		args = append(args, syscall.EscapeArg(a))
	}
	args = append(args, "-elevated")
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(args, " "))
	return windows.ShellExecute(0, verb, file, params, nil, windows.SW_SHOWNORMAL)
}

func stopAndDelete(s *mgr.Service) {
	s.Control(svc.Stop)
	for i := 0; i < 50; i++ {
		if st, err := s.Query(); err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.Delete()
	s.Close()
}

func firewallRule(exe string, add bool) {
	exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+svcName).Run()
	if add {
		exec.Command("netsh", "advfirewall", "firewall", "add", "rule", "name="+svcName,
			"dir=in", "action=allow", "program="+exe, "enable=yes", "profile=any").Run()
	}
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// installService копіює програму в Program Files, реєструє службу з автозапуском,
// відкриває її у брандмауері й запускає. Правило діє для всіх типів мереж: домашню
// мережу Windows часто позначає «публічною», і з вужчим правилом сервер був би
// недосяжний. Звузити можна в налаштуваннях брандмауера (правило «SimpleVNC»).
func installService(c svcConfig) (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(svcName); err == nil {
		stopAndDelete(s) // оновлення попереднього встановлення
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe := filepath.Join(installDir(), "SimpleVNC.exe")
	if !strings.EqualFold(self, exe) {
		if err := os.MkdirAll(installDir(), 0755); err != nil {
			return "", err
		}
		for i := 0; ; i++ { // старий робочий процес може ще завершуватися
			if err = copyFile(exe, self); err == nil || i == 20 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			return "", err
		}
	}
	if err := saveConfig(c); err != nil {
		return "", err
	}
	var s *mgr.Service
	for i := 0; ; i++ { // щойно видалена служба звільняє ім'я не миттєво
		s, err = m.CreateService(svcName, exe, mgr.Config{
			DisplayName: "SimpleVNC Server",
			Description: "Віддалений доступ до екрана цього комп'ютера за протоколом VNC.",
			StartType:   mgr.StartAutomatic,
		}, "-service")
		if err == nil || i == 20 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 60)
	firewallRule(exe, true)
	return exe, s.Start()
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err != nil {
		return fmt.Errorf("службу не встановлено")
	}
	stopAndDelete(s)
	restoreLeftoverMode() // службу зупинено примусово — звичайну роздільність повертаємо самі
	firewallRule("", false)
	os.Remove(configPath())
	time.Sleep(500 * time.Millisecond)
	os.RemoveAll(installDir())
	return nil
}

// serviceCommand виконує -install або -uninstall; повертає код завершення.
// given — параметри, задані явно: під час оновлення решта лишається як була.
func serviceCommand(install bool, c svcConfig, noPass, viaUAC bool, given map[string]bool) int {
	if !isElevated() {
		fmt.Println("Потрібні права адміністратора — Windows зараз попросить підтвердження.")
		if err := relaunchElevated(); err != nil {
			fmt.Println("Не вдалося отримати права адміністратора:", err)
			return 1
		}
		return 0
	}
	if viaUAC { // вікно відкрив UAC — не даємо йому закритися, доки не прочитано
		defer func() {
			fmt.Print("\nНатисніть Enter, щоб закрити вікно…")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}()
	}
	if !install {
		if err := uninstallService(); err != nil {
			fmt.Println("Не вдалося видалити службу:", err)
			return 1
		}
		fmt.Println("Службу SimpleVNC видалено.")
		return 0
	}
	keepPort, updating := false, false
	if old, err := loadConfig(); err == nil { // оновлення: чого не задано, те лишається як було
		updating = true
		if !given["p"] && !noPass {
			c.Password = old.Password
		}
		if !given["agentpass"] {
			c.AgentPassword = old.AgentPassword
		}
		if !given["agentview"] {
			c.AgentView = old.AgentView
		}
		if !given["scale"] && old.Scale != 0 {
			c.Scale = old.Scale
		}
		if !given["area"] && old.Area != "" {
			c.Area = old.Area
		}
		if !given["mode"] {
			c.Mode = old.Mode
		}
		if !given["port"] && old.Port != 0 {
			c.Port, keepPort = old.Port, true // порт зараз зайнятий самою службою — це нормально
		}
	}
	if c.Password == "" && !noPass {
		c.Password = randomPassword()
	}
	if !keepPort && (c.Port == 0 || !portFree(c.Port)) { // порт фіксуємо зараз, щоб він не змінювався між запусками
		for p := 5900; p < 6000; p++ {
			if portFree(p) {
				c.Port = p
				break
			}
		}
	}
	exe, err := installService(c)
	if err != nil {
		fmt.Println("Не вдалося встановити службу:", err)
		return 1
	}
	if updating {
		fmt.Println("Службу SimpleVNC оновлено. Налаштування, яких ви не задавали, лишилися як були.")
	}
	fmt.Println("Службу SimpleVNC", version, "встановлено й запущено. Вона стартує разом із Windows,")
	fmt.Println("показує вікна дозволу (UAC) та екран входу.")
	fmt.Println("  Програма: ", exe)
	fmt.Println("  Порт:     ", c.Port)
	if c.Password == "" {
		fmt.Println("  Пароль:    немає (будь-хто в мережі може підключитися!)")
	} else {
		fmt.Println("  Пароль:   ", c.Password, " — запишіть, більше він ніде не показується")
	}
	if c.AgentPassword != "" && c.Password != "" {
		mode := "керування без вікон дозволу Windows"
		if c.AgentView {
			mode = "лише перегляд"
		}
		fmt.Println("  Пароль для агентів:", c.AgentPassword, "—", mode)
	}
	fmt.Println("  Журнал:   ", logPath())
	fmt.Println("Видалити службу: SimpleVNC.exe -uninstall")
	return 0
}

func portFree(p int) bool {
	ln, err := listenTCP("", p)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// vncService — сама служба: тримає робочий процес у сеансі консолі.
type vncService struct{}

func (vncService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	var proc windows.Handle
	var session uint32
	kill := func() {
		if proc != 0 {
			windows.TerminateProcess(proc, 0)
			windows.CloseHandle(proc)
			proc = 0
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				kill()
				return false, 0
			}
		case <-tick.C:
			sid := windows.WTSGetActiveConsoleSessionId()
			if proc != 0 {
				ev, _ := windows.WaitForSingleObject(proc, 0)
				if ev == windows.WAIT_OBJECT_0 || sid != session {
					kill() // процес завершився або консоль перейшла в інший сеанс
				}
			}
			if proc == 0 && sid != 0xFFFFFFFF {
				h, err := startWorker(sid)
				if err != nil {
					logf("не вдалося запустити робочий процес у сеансі %d: %v", sid, err)
					continue
				}
				proc, session = h, sid
				logf("робочий процес запущено в сеансі %d", sid)
			}
		}
	}
}

// startWorker запускає цей самий exe з правами SYSTEM у сеансі sid.
func startWorker(sid uint32) (windows.Handle, error) {
	var tok windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|
		windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_SESSIONID|windows.TOKEN_ADJUST_DEFAULT, &tok)
	if err != nil {
		return 0, err
	}
	defer tok.Close()
	var dup windows.Token
	if err = windows.DuplicateTokenEx(tok, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation,
		windows.TokenPrimary, &dup); err != nil {
		return 0, err
	}
	defer dup.Close()
	if err = windows.SetTokenInformation(dup, windows.TokenSessionId, (*byte)(unsafe.Pointer(&sid)), 4); err != nil {
		return 0, err
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd, _ := windows.UTF16PtrFromString(syscall.EscapeArg(exe) + " -worker")
	desk, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: desk}
	var pi windows.ProcessInformation
	if err = windows.CreateProcessAsUser(dup, nil, cmd, nil, nil, false, windows.CREATE_NO_WINDOW, nil, nil, &si, &pi); err != nil {
		return 0, err
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}

func runService() {
	setupFileLog()
	logf("службу запущено")
	if err := svc.Run(svcName, vncService{}); err != nil {
		logf("служба завершилася з помилкою: %v", err)
	}
}
