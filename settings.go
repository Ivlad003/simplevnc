//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// Налаштування, змінені на сторінці підключення (область, розмір кадру,
// роздільність екрана ПК), зберігаються й діють після перезапуску. Служба тримає
// їх у своєму файлі налаштувань, звичайна програма — у профілі користувача.
// Параметри командного рядка мають перевагу над збереженим.

type savedSettings struct {
	Scale int    `json:"scale,omitempty"`
	Area  string `json:"area,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

func settingsPath() string {
	if p := os.Getenv("SIMPLEVNC_SETTINGS"); p != "" {
		return p
	}
	d, err := os.UserConfigDir() // %AppData%
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "SimpleVNC", "settings.json")
}

func loadSettings() (s savedSettings) {
	if b, err := os.ReadFile(settingsPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

var settingsMu sync.Mutex

// saveSettings запам'ятовує поточні налаштування трансляції.
func (s *Server) saveSettings() {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	cur := savedSettings{Scale: int(s.scr.scale.Load()), Area: areaID(), Mode: s.disp.wanted()}
	var err error
	if serviceMode {
		var c svcConfig
		if c, err = loadConfig(); err == nil {
			c.Scale, c.Area, c.Mode = cur.Scale, cur.Area, cur.Mode
			err = saveConfig(c)
		}
	} else {
		p := settingsPath()
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			b, _ := json.Marshal(cur)
			err = os.WriteFile(p, b, 0o600)
		}
	}
	if err != nil {
		logf("не вдалося зберегти налаштування: %v", err)
	}
}

// Перезапуск зі сторінки підключення. Службу піднімає знову її диспетчер; звичайна
// програма запускає себе з тими самими параметрами й передає новому процесу свій
// номер (той чекає, доки звільняться порти) та пароль, щоб він не змінився.
const (
	envRestartPID  = "SIMPLEVNC_RESTART_PID"
	envRestartPass = "SIMPLEVNC_RESTART_PASSWORD"
)

// bootID відрізняє один запуск сервера від іншого: сторінка бачить, що перезапуск відбувся.
var bootID = strconv.FormatInt(time.Now().UnixNano(), 36)

func (s *Server) restart() {
	logf("перезапуск зі сторінки підключення")
	releaseAllInput()
	s.disp.shutdown() // екрану — звичайну роздільність; після підключення вона ввімкнеться знову
	if !serviceMode {
		exe, err := os.Executable()
		if err == nil {
			cmd := exec.Command(exe, os.Args[1:]...)
			cmd.Env = append(os.Environ(), envRestartPID+"="+strconv.Itoa(os.Getpid()), envRestartPass+"="+s.password)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			err = cmd.Start()
		}
		if err != nil {
			logf("не вдалося перезапуститися: %v", err)
			return
		}
	}
	os.Exit(0)
}

// afterRestart повідомляє, чи цей процес запущено перезапуском, і якщо так — чекає,
// доки попередній завершиться, та повертає його пароль.
func afterRestart() (restarted bool, password string) {
	pid, err := strconv.Atoi(os.Getenv(envRestartPID))
	if err != nil || pid <= 0 {
		return false, ""
	}
	password = os.Getenv(envRestartPass)
	os.Unsetenv(envRestartPID) // дочірнім процесам це ні до чого
	os.Unsetenv(envRestartPass)
	if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)); err == nil {
		windows.WaitForSingleObject(h, 10000)
		windows.CloseHandle(h)
	}
	return true, password
}

func (u *webUI) restart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	go func() {
		time.Sleep(300 * time.Millisecond) // дати відповіді дійти до браузера
		u.srv.restart()
	}()
}
