//go:build windows

package main

import (
	"fmt"
	"sort"
	"strings"
)

// Клієнт VNC шле не клавіші, а символи: «a», «ф», «;». Зазвичай сервер уводить
// саме цей символ, і мову визначає клавіатура клієнта. Але якщо у Windows увімкнено
// нелатинську розкладку (індикатор показує УКР), а клієнт і далі шле латиницю,
// людина явно перемкнула мову у Windows. Тоді натискаємо «фізичну» клавішу — ту,
// на якій цей символ стоїть в американській розкладці, — і символ обирає Windows.
// Так мова змінюється незалежно від того, де її перемкнули: на клієнті чи на ПК.

const keyScancode = 0x0008 // KEYEVENTF_SCANCODE

type scanKey struct {
	sc    uint16
	shift bool
}

// usScan — скан-коди клавіш американської розкладки для друкованих символів ASCII.
var usScan = map[rune]scanKey{}

func init() {
	rows := []struct {
		base           uint16
		plain, shifted string
	}{
		{0x02, "1234567890-=", "!@#$%^&*()_+"},
		{0x10, "qwertyuiop[]", "QWERTYUIOP{}"},
		{0x1E, "asdfghjkl;'", "ASDFGHJKL:\""},
		{0x29, "`", "~"},
		{0x2B, "\\", "|"},
		{0x2C, "zxcvbnm,./", "ZXCVBNM<>?"},
		{0x39, " ", ""},
	}
	for _, row := range rows {
		for i, r := range row.plain {
			usScan[r] = scanKey{row.base + uint16(i), false}
		}
		for i, r := range row.shifted {
			usScan[r] = scanKey{row.base + uint16(i), true}
		}
	}
}

var modNames = map[uint32]string{
	0xFFE1: "Shift_L", 0xFFE2: "Shift_R", 0xFFE3: "Control_L", 0xFFE4: "Control_R",
	0xFFE7: "Meta_L", 0xFFE8: "Meta_R", 0xFFE9: "Alt_L", 0xFFEA: "Alt_R",
	0xFFEB: "Super_L", 0xFFEC: "Super_R", 0xFFE5: "Caps_Lock", 0xFE03: "ISO_Level3_Shift",
}

// keyStats рахує лише види клавіш — без самих символів і без порядку, тож набраний
// текст із журналу відновити неможливо. Потрібно, щоб бачити, що шле клієнт.
type keyStats struct {
	latin, cyr, other, phys int
	mods                    map[string]int
}

func (s *keyStats) mod(name string) {
	if s.mods == nil {
		s.mods = map[string]int{}
	}
	s.mods[name]++
}

func (s *keyStats) empty() bool {
	return s.latin+s.cyr+s.other == 0 && len(s.mods) == 0
}

func (s *keyStats) String() string {
	names := make([]string, 0, len(s.mods))
	for n := range s.mods {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		names[i] = fmt.Sprintf("%s×%d", n, s.mods[n])
	}
	m := "немає"
	if len(names) > 0 {
		m = strings.Join(names, " ")
	}
	return fmt.Sprintf("латинських літер %d, нелатинських %d, інших символів %d, з них фізичними клавішами %d; модифікатори: %s",
		s.latin, s.cyr, s.other, s.phys, m)
}
