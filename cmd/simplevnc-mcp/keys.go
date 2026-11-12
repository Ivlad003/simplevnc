package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Назви клавіш для інструмента press_keys → коди клавіш VNC (keysym).

var modifierKeys = map[string]uint32{
	"ctrl": 0xFFE3, "control": 0xFFE3,
	"alt": 0xFFE9, "option": 0xFFE9,
	"shift": 0xFFE1,
	"win":   0xFFEB, "windows": 0xFFEB, "super": 0xFFEB, "cmd": 0xFFEB, "command": 0xFFEB, "meta": 0xFFEB,
}

var namedKeys = map[string]uint32{
	"enter": 0xFF0D, "return": 0xFF0D, "tab": 0xFF09, "esc": 0xFF1B, "escape": 0xFF1B,
	"backspace": 0xFF08, "delete": 0xFFFF, "del": 0xFFFF, "insert": 0xFF63, "ins": 0xFF63,
	"home": 0xFF50, "end": 0xFF57, "pageup": 0xFF55, "pgup": 0xFF55, "pagedown": 0xFF56, "pgdn": 0xFF56,
	"left": 0xFF51, "up": 0xFF52, "right": 0xFF53, "down": 0xFF54,
	"space": 0x20, "capslock": 0xFFE5, "printscreen": 0xFF61, "menu": 0xFF67,
	"plus": '+', "minus": '-',
}

// runeKeysym — keysym для друкованого символу.
func runeKeysym(r rune) uint32 {
	switch {
	case r == '\n':
		return 0xFF0D
	case r == '\t':
		return 0xFF09
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return uint32(r)
	}
	return 0x01000000 | uint32(r) // Unicode
}

// parseCombo розбирає «ctrl+shift+t» на модифікатори та основну клавішу.
// Комбінація з самих модифікаторів («alt+shift») теж дозволена.
func parseCombo(s string) (mods []uint32, key uint32, err error) {
	parts := strings.Split(strings.TrimSpace(s), "+")
	if s = strings.TrimSpace(s); s == "+" {
		parts = []string{"plus"}
	}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		low := strings.ToLower(p)
		if m, ok := modifierKeys[low]; ok {
			mods = append(mods, m)
			continue
		}
		if i != len(parts)-1 {
			return nil, 0, fmt.Errorf("unknown modifier %q (use ctrl, alt, shift, win)", p)
		}
		if k, ok := namedKeys[low]; ok {
			key = k
		} else if n, e := strconv.Atoi(strings.TrimPrefix(low, "f")); e == nil && strings.HasPrefix(low, "f") && n >= 1 && n <= 24 {
			key = 0xFFBE + uint32(n-1)
		} else if utf8.RuneCountInString(p) == 1 {
			r, _ := utf8.DecodeRuneInString(p)
			if len(mods) > 0 { // «ctrl+C» означає клавішу C, а не велику літеру
				r, _ = utf8.DecodeRuneInString(low)
			}
			key = runeKeysym(r)
		} else {
			return nil, 0, fmt.Errorf("unknown key %q", p)
		}
	}
	if key == 0 && len(mods) == 0 {
		return nil, 0, fmt.Errorf("empty key combination")
	}
	return mods, key, nil
}
