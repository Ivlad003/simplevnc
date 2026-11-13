//go:build windows

package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Область трансляції: увесь віртуальний екран, один монітор, половина екрана
// або довільний прямокутник. Від неї залежать захоплення й координати миші.

type areaRect struct{ x, y, w, h int }

type monitor struct {
	areaRect
	primary bool
	device  [32]uint16 // назва дисплея у Windows — щоб міняти його роздільність
}

var (
	pEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")

	monMu    sync.Mutex
	monList  []monitor
	monAt    time.Time
	monFound []monitor
	monEnum  = syscall.NewCallback(monitorEnumProc) // зворотний виклик створюємо один раз
)

// monitorInfo — MONITORINFOEXW.
type monitorInfo struct {
	size      uint32
	mon, work [4]int32
	flags     uint32
	device    [32]uint16
}

func monitorEnumProc(hMon, hdc, rc, data uintptr) uintptr {
	mi := monitorInfo{size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := pGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
		monFound = append(monFound, monitor{
			areaRect: areaRect{int(mi.mon[0]), int(mi.mon[1]), int(mi.mon[2] - mi.mon[0]), int(mi.mon[3] - mi.mon[1])},
			primary:  mi.flags&1 != 0,
			device:   mi.device,
		})
	}
	return 1
}

// monitors повертає монітори: спершу основний, далі зліва направо.
func monitors() []monitor {
	monMu.Lock()
	defer monMu.Unlock()
	if time.Since(monAt) < time.Second && monList != nil {
		return monList
	}
	monFound = nil
	pEnumDisplayMonitors.Call(0, 0, monEnum, 0)
	sort.SliceStable(monFound, func(i, j int) bool {
		a, b := monFound[i], monFound[j]
		if a.primary != b.primary {
			return a.primary
		}
		return a.x < b.x
	})
	monList, monAt = monFound, time.Now()
	return monList
}

type areaSel struct {
	id   string // all, left, right, m1…, або x,y,w,h
	kind byte   // 'a' усе, 'l' ліва половина, 'r' права, 'm' монітор, 'c' довільний прямокутник
	idx  int
	r    areaRect
}

var curArea atomic.Pointer[areaSel]

func parseArea(id string) (*areaSel, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	switch {
	case id == "" || id == "all":
		return &areaSel{id: "all", kind: 'a'}, nil
	case id == "left":
		return &areaSel{id: id, kind: 'l'}, nil
	case id == "right":
		return &areaSel{id: id, kind: 'r'}, nil
	case strings.HasPrefix(id, "m"):
		n, err := strconv.Atoi(id[1:])
		if err != nil || n < 1 {
			return nil, fmt.Errorf("невідомий монітор %q", id)
		}
		return &areaSel{id: id, kind: 'm', idx: n - 1}, nil
	}
	var r areaRect
	if _, err := fmt.Sscanf(id, "%d,%d,%d,%d", &r.x, &r.y, &r.w, &r.h); err != nil || r.w < 64 || r.h < 64 {
		return nil, fmt.Errorf("невідома область %q", id)
	}
	return &areaSel{id: id, kind: 'c', r: r}, nil
}

// captureArea — прямокутник трансляції в координатах віртуального екрана.
func captureArea() (x, y, w, h int) {
	vx, vy, vw, vh := virtualScreen()
	sel := curArea.Load()
	if sel == nil {
		return vx, vy, vw, vh
	}
	switch sel.kind {
	case 'l':
		return vx, vy, vw / 2, vh
	case 'r':
		return vx + vw/2, vy, vw - vw/2, vh
	case 'm':
		if ms := monitors(); sel.idx < len(ms) {
			m := ms[sel.idx]
			return m.x, m.y, m.w, m.h
		}
	case 'c': // обрізаємо до екрана
		r := sel.r
		x0, y0 := max(r.x, vx), max(r.y, vy)
		x1, y1 := min(r.x+r.w, vx+vw), min(r.y+r.h, vy+vh)
		if x1-x0 >= 64 && y1-y0 >= 64 {
			return x0, y0, x1 - x0, y1 - y0
		}
	}
	return vx, vy, vw, vh
}

func areaID() string {
	if sel := curArea.Load(); sel != nil {
		return sel.id
	}
	return "all"
}

// setArea змінює область на льоту; клієнти отримують новий розмір екрана.
func (s *Screen) setArea(id string) error {
	sel, err := parseArea(id)
	if err != nil {
		return err
	}
	if sel.kind == 'm' && sel.idx >= len(monitors()) {
		return fmt.Errorf("монітора %d немає", sel.idx+1)
	}
	curArea.Store(sel)
	s.stale.Store(true)
	s.poke()
	return nil
}

// areaChoice — варіант області для сторінки підключення.
type areaChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func areaChoices() []areaChoice {
	_, _, vw, vh := virtualScreen()
	out := []areaChoice{{"all", fmt.Sprintf("Увесь екран · %d×%d", vw, vh)}}
	if ms := monitors(); len(ms) > 1 {
		for i, m := range ms {
			name := fmt.Sprintf("Монітор %d · %d×%d", i+1, m.w, m.h)
			if m.primary {
				name += " (основний)"
			}
			out = append(out, areaChoice{"m" + strconv.Itoa(i+1), name})
		}
	}
	out = append(out,
		areaChoice{"left", fmt.Sprintf("Ліва половина · %d×%d", vw/2, vh)},
		areaChoice{"right", fmt.Sprintf("Права половина · %d×%d", vw-vw/2, vh)})
	return out
}
