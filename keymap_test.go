//go:build windows

package main

import (
	"testing"
	"unsafe"
)

// Перевіряє таблицю фізичних клавіш на справжній українській розкладці Windows:
// нічого не натискає, лише питає систему, який символ дала б клавіша.
func TestPhysicalKeysTypeUkrainian(t *testing.T) {
	toUnicodeEx := user32.NewProc("ToUnicodeEx")
	mapVirtualKeyEx := user32.NewProc("MapVirtualKeyExW")

	var list [16]uintptr
	n, _, _ := pGetKeyboardLayoutList.Call(16, uintptr(unsafe.Pointer(&list[0])))
	var hkl uintptr
	for _, h := range list[:n] {
		if h&0xffff == 0x0422 { // мова розкладки — українська
			hkl = h
		}
	}
	if hkl == 0 {
		t.Skip("українську розкладку не встановлено")
	}

	typed := func(r rune) string {
		k, ok := usScan[r]
		if !ok {
			t.Fatalf("немає скан-коду для %q", r)
		}
		vk, _, _ := mapVirtualKeyEx.Call(uintptr(k.sc), 3, hkl) // скан-код → віртуальна клавіша
		var state [256]byte
		if k.shift {
			state[0x10] = 0x80
		}
		var buf [8]uint16
		// прапорець 4: не змінювати стан клавіатури системи
		got, _, _ := toUnicodeEx.Call(vk, uintptr(k.sc), uintptr(unsafe.Pointer(&state[0])),
			uintptr(unsafe.Pointer(&buf[0])), 8, 4, hkl)
		if int32(got) < 1 {
			return ""
		}
		return string(rune(buf[0]))
	}

	want := map[rune]string{
		'q': "й", 'a': "ф", 's': "і", 'z': "я", 'f': "а", 'A': "Ф", 'S': "І",
		';': "ж", '\'': "є", '[': "х", ']': "ї", ',': "б", '.': "ю", ':': "Ж", '<': "Б",
		'1': "1", ' ': " ",
	}
	for r, w := range want {
		if got := typed(r); got != w {
			t.Errorf("клавіша %q в українській розкладці дала %q, очікував %q", r, got, w)
		}
	}
}
