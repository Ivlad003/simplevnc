//go:build windows

package main

import (
	"testing"
	"unsafe"
)

func TestDevModeLayout(t *testing.T) {
	var dm devMode
	if n := unsafe.Sizeof(dm); n != 220 {
		t.Fatalf("DEVMODEW має займати 220 байт, а не %d", n)
	}
	for name, got := range map[string]uintptr{
		"fields":           unsafe.Offsetof(dm.fields),
		"bitsPerPel":       unsafe.Offsetof(dm.bitsPerPel),
		"pelsWidth":        unsafe.Offsetof(dm.pelsWidth),
		"displayFrequency": unsafe.Offsetof(dm.displayFrequency),
	} {
		want := map[string]uintptr{"fields": 72, "bitsPerPel": 168, "pelsWidth": 172, "displayFrequency": 184}[name]
		if got != want {
			t.Errorf("зсув поля %s = %d, має бути %d", name, got, want)
		}
	}
	if n := unsafe.Sizeof(monitorInfo{}); n != 104 {
		t.Fatalf("MONITORINFOEXW має займати 104 байти, а не %d", n)
	}
}

func TestRatioName(t *testing.T) {
	for _, c := range []struct {
		w, h int
		want string
	}{
		{3440, 1440, "21:9"}, {2560, 1080, "21:9"}, {3840, 1600, "21:9"}, {5120, 1440, "32:9"},
		{1920, 1080, "16:9"}, {1366, 768, "16:9"}, {2560, 1440, "16:9"},
		{1920, 1200, "16:10"}, {1680, 1050, "16:10"}, {1280, 800, "16:10"},
		{1280, 768, "5:3"}, {2160, 1440, "3:2"},
		{1600, 1200, "4:3"}, {1024, 768, "4:3"}, {1440, 1080, "4:3"}, {1280, 1024, "5:4"},
		{1600, 1024, ""}, {0, 0, ""},
	} {
		if got := ratioName(c.w, c.h); got != c.want {
			t.Errorf("ratioName(%d, %d) = %q, має бути %q", c.w, c.h, got, c.want)
		}
	}
}

func TestParseModeID(t *testing.T) {
	for _, c := range []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"1920x1200", 1920, 1200, true}, {" 1920X1200 ", 1920, 1200, true}, {"1920×1200", 1920, 1200, true},
		{"native", 0, 0, false}, {"1920", 0, 0, false}, {"x1200", 0, 0, false}, {"-5x10", 0, 0, false},
	} {
		w, h, ok := parseModeID(c.in)
		if ok != c.ok || (ok && (w != c.w || h != c.h)) {
			t.Errorf("parseModeID(%q) = %d, %d, %v", c.in, w, h, ok)
		}
	}
}

func TestHzRank(t *testing.T) {
	// та сама частота найкраща, далі нижчі (ближча краща), вищі — в останню чергу
	if !(hzRank(60, 60) < hzRank(59, 60) && hzRank(59, 60) < hzRank(50, 60) && hzRank(50, 60) < hzRank(75, 60)) {
		t.Fatal("порядок частот оновлення неправильний")
	}
}

// Кожен режим зі списку має проходити перевірку Windows. CDS_TEST нічого не
// перемикає, тож екран під час тесту не змінюється.
func TestListedModesAreAccepted(t *testing.T) {
	setDPIAware()
	dev := displayDevice()
	cw, ch, _ := queryMode(dev, enumCurrentSettings)
	modes := listModes()
	if cw == 0 || len(modes) == 0 {
		t.Skip("немає дисплея")
	}
	seen, current := map[[2]int]bool{}, false
	for i, m := range modes {
		if seen[[2]int{m.W, m.H}] {
			t.Errorf("режим %dx%d повторюється", m.W, m.H)
		}
		seen[[2]int{m.W, m.H}] = true
		if i > 0 && m.W*m.H > modes[i-1].W*modes[i-1].H {
			t.Errorf("список не впорядковано за розміром: %dx%d після %dx%d", m.W, m.H, modes[i-1].W, modes[i-1].H)
		}
		current = current || (m.W == cw && m.H == ch)
		var d unsafe.Pointer
		if dev != nil {
			d = unsafe.Pointer(&dev[0])
		}
		dm := m.dm
		r, _, _ := pChangeDisplaySettingsExW.Call(uintptr(d), uintptr(unsafe.Pointer(&dm)), 0, cdsTest, 0)
		if int32(uint32(r)) != 0 {
			t.Errorf("Windows відхиляє режим %dx%d@%d (код %d)", m.W, m.H, m.Hz, int32(uint32(r)))
		}
	}
	if !current {
		t.Errorf("поточного режиму %dx%d немає у списку з %d режимів", cw, ch, len(modes))
	}
	if findMode(modeID(cw, ch)) == nil || findMode("123x45") != nil {
		t.Error("findMode шукає неправильно")
	}
	t.Logf("%d режимів, зараз %dx%d", len(modes), cw, ch)
}
