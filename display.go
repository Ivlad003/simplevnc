//go:build windows

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Роздільність екрана ПК на час підключення. Монітор ПК часто має інші пропорції,
// ніж пристрій, з якого дивляться (широкий 21:9 проти 16:10 у ноутбука чи 4:3 у
// планшета), і картинка виходить вузькою смужкою. Поки хтось підключений, екран
// ПК можна перемкнути в один із режимів, які підтримує монітор; після відключення
// повертається звичайний. У реєстр зміна не пишеться: після перезавантаження
// екран у будь-якому разі звичайний.

var (
	pEnumDisplaySettingsW     = user32.NewProc("EnumDisplaySettingsW")
	pChangeDisplaySettingsExW = user32.NewProc("ChangeDisplaySettingsExW")
)

// devMode — DEVMODEW у варіанті для дисплея (220 байт).
type devMode struct {
	deviceName            [32]uint16
	specVersion           uint16
	driverVersion         uint16
	size                  uint16
	driverExtra           uint16
	fields                uint32
	posX, posY            int32
	orientation           uint32
	fixedOutput           uint32
	color, duplex         int16
	yResolution, ttOption int16
	collate               int16
	formName              [32]uint16
	logPixels             uint16
	bitsPerPel            uint32
	pelsWidth, pelsHeight uint32
	displayFlags          uint32
	displayFrequency      uint32
	reserved              [8]uint32
}

const (
	enumCurrentSettings  = 0xFFFFFFFF // режим, що діє зараз
	enumRegistrySettings = 0xFFFFFFFE // «звичайний» режим, збережений у Windows
	cdsTest              = 2          // лише перевірити, чи режим можливий
	dmInterlaced         = 2
)

// dispMode — режим екрана, який підтримують монітор і відеокарта.
type dispMode struct {
	W, H, Hz int
	dm       devMode
}

func modeID(w, h int) string { return strconv.Itoa(w) + "x" + strconv.Itoa(h) }

// parseModeID розбирає «1920x1200» (годиться й «1920×1200»).
func parseModeID(id string) (w, h int, ok bool) {
	id = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(id)), "×", "x")
	a, b, found := strings.Cut(id, "x")
	if !found {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	return w, h, err1 == nil && err2 == nil && w > 0 && h > 0
}

// ratioName — звична назва пропорцій екрана або "", якщо вони нестандартні.
func ratioName(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := float64(w) / float64(h)
	best, bestD := "", 0.02 // приймаємо відхилення до 2%
	for _, k := range []struct {
		v float64
		n string
	}{{32.0 / 9, "32:9"}, {2.385, "21:9"}, {16.0 / 9, "16:9"}, {5.0 / 3, "5:3"}, {1.6, "16:10"},
		{1.5, "3:2"}, {4.0 / 3, "4:3"}, {1.25, "5:4"}} {
		if d := math.Abs(r/k.v - 1); d < bestD {
			best, bestD = k.n, d
		}
	}
	return best
}

// displayDevice — дисплей, режим якого міняємо: монітор, який транслюється, або
// основний. nil — основний дисплей за замовчуванням.
func displayDevice() *[32]uint16 {
	ms := monitors()
	if len(ms) == 0 {
		return nil
	}
	i := 0
	if sel := curArea.Load(); sel != nil && sel.kind == 'm' && sel.idx < len(ms) {
		i = sel.idx
	}
	d := ms[i].device
	if d[0] == 0 {
		return nil
	}
	return &d
}

var pEnumDisplayDevicesW = user32.NewProc("EnumDisplayDevicesW")

// monitorActive — чи до цього виходу відеокарти зараз під'єднано ввімкнений монітор.
// Коли монітор вимкнено чи від'єднано, Windows лишає робочому столу його розмір, але
// кадрів на екран не виводить.
func monitorActive(dev *[32]uint16) bool {
	if dev == nil {
		return true // пристрій невідомий — не вгадуємо
	}
	var dd struct {
		cb    uint32
		name  [32]uint16
		str   [128]uint16
		flags uint32
		id    [128]uint16
		key   [128]uint16
	}
	dd.cb = uint32(unsafe.Sizeof(dd))
	r, _, _ := pEnumDisplayDevicesW.Call(uintptr(unsafe.Pointer(&dev[0])), 0, uintptr(unsafe.Pointer(&dd)), 0)
	return r != 0 && dd.flags&1 != 0 // DISPLAY_DEVICE_ACTIVE
}

func enumSettings(dev *[32]uint16, n uint32, dm *devMode) bool {
	var d unsafe.Pointer
	if dev != nil {
		d = unsafe.Pointer(&dev[0])
	}
	dm.size = uint16(unsafe.Sizeof(*dm))
	r, _, _ := pEnumDisplaySettingsW.Call(uintptr(d), uintptr(n), uintptr(unsafe.Pointer(dm)))
	return r != 0
}

// queryMode повертає режим, що діє зараз (enumCurrentSettings), або звичайний
// (enumRegistrySettings).
func queryMode(dev *[32]uint16, which uint32) (w, h, hz int) {
	var dm devMode
	if !enumSettings(dev, which, &dm) {
		return 0, 0, 0
	}
	return int(dm.pelsWidth), int(dm.pelsHeight), int(dm.displayFrequency)
}

// hzRank — наскільки частота оновлення гірша за поточну (менше — краще): та сама,
// далі нижчі, і лише потім вищі — їх монітор може не витримати.
func hzRank(hz, cur int) int {
	switch {
	case hz == cur:
		return 0
	case hz < cur:
		return cur - hz
	}
	return 1000 + hz - cur
}

var modeCache struct {
	mu   sync.Mutex
	at   time.Time
	dev  [32]uint16
	list []dispMode
}

// listModes — режими дисплея без повторів, від найбільшого до найменшого.
func listModes() []dispMode {
	dev := displayDevice()
	var key [32]uint16
	if dev != nil {
		key = *dev
	}
	modeCache.mu.Lock()
	defer modeCache.mu.Unlock()
	if modeCache.list != nil && key == modeCache.dev && time.Since(modeCache.at) < 5*time.Second {
		return modeCache.list
	}
	cw, ch, chz := queryMode(dev, enumCurrentSettings)
	best := map[[2]int]dispMode{}
	for i := uint32(0); i < 4096; i++ {
		var dm devMode
		if !enumSettings(dev, i, &dm) {
			break
		}
		w, h, hz := int(dm.pelsWidth), int(dm.pelsHeight), int(dm.displayFrequency)
		if dm.bitsPerPel != 32 || dm.displayFlags&dmInterlaced != 0 || w < 800 || h < 600 {
			continue
		}
		if cw > 0 && (w < h) != (cw < ch) {
			continue // режими для повернутого екрана
		}
		k := [2]int{w, h}
		if old, ok := best[k]; !ok || hzRank(hz, chz) < hzRank(old.Hz, chz) {
			best[k] = dispMode{w, h, hz, dm}
		}
	}
	list := make([]dispMode, 0, len(best))
	for _, m := range best {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool {
		if a, b := list[i].W*list[i].H, list[j].W*list[j].H; a != b {
			return a > b
		}
		return list[i].W > list[j].W
	})
	modeCache.list, modeCache.dev, modeCache.at = list, key, time.Now()
	return list
}

func findMode(id string) *dispMode {
	w, h, ok := parseModeID(id)
	if !ok {
		return nil
	}
	for _, m := range listModes() {
		if m.W == w && m.H == h {
			m := m
			return &m
		}
	}
	return nil
}

// changeMode перемикає дисплей у режим dm; dm == nil повертає звичайний режим.
// Зміна тимчасова: у реєстр Windows не записується.
func changeMode(dev *[32]uint16, dm *devMode) error {
	call := func(flags uintptr) int32 {
		var d unsafe.Pointer
		if dev != nil {
			d = unsafe.Pointer(&dev[0])
		}
		r, _, _ := pChangeDisplaySettingsExW.Call(uintptr(d), uintptr(unsafe.Pointer(dm)), 0, flags, 0)
		return int32(uint32(r))
	}
	if dm != nil {
		if r := call(cdsTest); r != 0 {
			return fmt.Errorf("екран не може працювати в цьому режимі (код %d)", r)
		}
	}
	if r := call(0); r != 0 {
		return fmt.Errorf("Windows не змінила режим екрана (код %d)", r)
	}
	return nil
}

// Позначка «режим змінили ми»: якщо процес зупинили, не давши повернути звичайний
// режим (оновлення служби, збій), наступний запуск доведе справу до кінця. У файлі —
// назва дисплея.
func modeMarker(service bool) string {
	if service {
		return filepath.Join(dataDir(), "mode-applied")
	}
	return filepath.Join(os.TempDir(), "simplevnc-mode-applied")
}

func devFromName(name string) *[32]uint16 {
	u, err := syscall.UTF16FromString(strings.TrimSpace(name))
	if err != nil || len(u) < 2 || len(u) > 32 {
		return nil
	}
	var d [32]uint16
	copy(d[:], u)
	return &d
}

// restoreLeftoverMode повертає звичайний режим, якщо позначка лишилася від служби,
// яку щойно зупинили. Викликається під час видалення служби.
func restoreLeftoverMode() {
	p := modeMarker(true)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	changeMode(devFromName(string(b)), nil)
	os.Remove(p)
}

// displayCtl стежить, щоб обраний режим діяв, лише поки хтось підключений.
type displayCtl struct {
	mu      sync.Mutex
	scr     *Screen
	want    string        // «1920x1200» або "" — екран не чіпаємо
	applied bool          // режим змінили ми, його треба повернути
	dev     *[32]uint16   // дисплей, який змінили
	timer   *time.Timer   // відкладене повернення після відключення останнього клієнта
	delay   time.Duration // скільки чекати: клієнт може просто перепідключатися
}

func newDisplayCtl(scr *Screen, want string) *displayCtl {
	d := &displayCtl{scr: scr, delay: time.Minute}
	if s, err := strconv.Atoi(os.Getenv("SIMPLEVNC_RESTORE_DELAY")); err == nil && s >= 0 { // для перевірок
		d.delay = time.Duration(s) * time.Second
	}
	if b, err := os.ReadFile(modeMarker(serviceMode)); err == nil {
		d.applied, d.dev = true, devFromName(string(b)) // попередній запуск не повернув режим
	}
	if want != "" && want != "native" {
		if err := d.set(want); err != nil {
			logf("%v — роздільність екрана не чіпаю", err)
		}
	}
	scr.onClients = d.sync
	go d.sync()
	return d
}

// viewers — скільки підключено людей. Сеанси агентів не рахуємо: вони бачать екран
// таким, яким він є, і не мають перемикати його в того, хто сидить за ПК.
func (d *displayCtl) viewers() int {
	n := 0
	for _, c := range d.scr.allClients() {
		if !c.agent {
			n++
		}
	}
	return n
}

// set обирає режим на час підключення; "" або "native" — не змінювати екран.
func (d *displayCtl) set(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id == "" || id == "native" {
		d.want = ""
		if d.applied {
			return d.restoreLocked()
		}
		return nil
	}
	m := findMode(id)
	if m == nil {
		return fmt.Errorf("екран не підтримує роздільність %q", id)
	}
	d.want = modeID(m.W, m.H)
	if d.viewers() > 0 {
		return d.applyLocked()
	}
	return nil
}

// wanted — режим, обраний на час підключення, або "".
func (d *displayCtl) wanted() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.want
}

// sync узгоджує екран із кількістю підключених; викликається, коли вона змінюється.
func (d *displayCtl) sync() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.viewers() > 0 {
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		if d.want != "" {
			d.applyLocked()
		}
		return
	}
	if d.applied && d.timer == nil {
		d.timer = time.AfterFunc(d.delay, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.timer = nil
			if d.applied && d.viewers() == 0 {
				d.restoreLocked()
			}
		})
	}
}

func (d *displayCtl) applyLocked() error {
	m := findMode(d.want)
	if m == nil {
		return fmt.Errorf("екран більше не підтримує роздільність %s", d.want)
	}
	dev := displayDevice()
	if w, h, _ := queryMode(dev, enumCurrentSettings); w == m.W && h == m.H {
		return nil // уже діє
	}
	if d.applied && d.dev != nil && (dev == nil || *dev != *d.dev) {
		d.restoreLocked() // транслюється вже інший монітор — попередній повертаємо
	}
	if err := changeMode(dev, &m.dm); err != nil {
		logf("роздільність %s: %v", d.want, err)
		return err
	}
	d.applied, d.dev = true, dev
	name := ""
	if dev != nil {
		name = syscall.UTF16ToString(dev[:])
	}
	os.WriteFile(modeMarker(serviceMode), []byte(name), 0o600)
	logf("екран ПК перемкнуто на %d×%d на час підключення", m.W, m.H)
	d.scr.stale.Store(true)
	d.scr.poke()
	return nil
}

func (d *displayCtl) restoreLocked() error {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	err := changeMode(d.dev, nil)
	d.applied, d.dev = false, nil
	os.Remove(modeMarker(serviceMode))
	if err != nil {
		logf("не вдалося повернути звичайну роздільність: %v", err)
		return err
	}
	logf("екрану ПК повернуто звичайну роздільність")
	d.scr.stale.Store(true)
	d.scr.poke()
	return nil
}

// shutdown повертає звичайний режим перед завершенням програми.
func (d *displayCtl) shutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.applied {
		d.restoreLocked()
	}
}

// modeChoice — режим для сторінки підключення.
type modeChoice struct {
	ID    string `json:"id"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Ratio string `json:"ratio"`
}

type displayStatus struct {
	Current string       `json:"current"` // що діє зараз, «1920×1200»
	Native  string       `json:"native"`  // звичайний режим
	Want    string       `json:"want"`    // обраний на час підключення або ""
	Applied bool         `json:"applied"` // зараз діє змінений режим
	Modes   []modeChoice `json:"modes"`
}

func (d *displayCtl) status() displayStatus {
	d.mu.Lock()
	st := displayStatus{Want: d.want, Applied: d.applied, Modes: []modeChoice{}}
	d.mu.Unlock()
	dev := displayDevice()
	if w, h, _ := queryMode(dev, enumCurrentSettings); w > 0 {
		st.Current = fmt.Sprintf("%d×%d", w, h)
	}
	if w, h, _ := queryMode(dev, enumRegistrySettings); w > 0 {
		st.Native = modeID(w, h)
	}
	for _, m := range listModes() {
		st.Modes = append(st.Modes, modeChoice{modeID(m.W, m.H), m.W, m.H, ratioName(m.W, m.H)})
	}
	return st
}
