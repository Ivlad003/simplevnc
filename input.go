//go:build windows

package main

import "unsafe"

// keyDown — що саме було натиснуто для певного keysym (щоб так само відпустити).
type keyDown struct {
	vk, scan uint16
	ext      bool
	uni      [2]uint16 // символ(и) UTF-16 для KEYEVENTF_UNICODE
	nUni     int
}

type inputState struct {
	buttons byte
	keys    map[uint32]keyDown
}

type vkDef struct {
	vk  uint16
	ext bool
}

var specialKeys = map[uint32]vkDef{
	0xFF08: {0x08, false}, 0xFF09: {0x09, false}, 0xFE20: {0x09, false},
	0xFF0D: {0x0D, false}, 0xFF1B: {0x1B, false}, 0xFF13: {0x13, false},
	0xFF14: {0x91, false}, 0xFF15: {0x2C, true}, 0xFF61: {0x2C, true},
	0xFF63: {0x2D, true}, 0xFFFF: {0x2E, true},
	0xFF50: {0x24, true}, 0xFF57: {0x23, true}, 0xFF55: {0x21, true}, 0xFF56: {0x22, true},
	0xFF51: {0x25, true}, 0xFF52: {0x26, true}, 0xFF53: {0x27, true}, 0xFF54: {0x28, true},
	0xFF67: {0x5D, true}, 0xFF7F: {0x90, true}, 0xFFE5: {0x14, false},
	0xFFE1: {0xA0, false}, 0xFFE2: {0xA1, false},
	0xFFE3: {0xA2, false}, 0xFFE4: {0xA3, true},
	0xFFE9: {0xA4, false}, 0xFFEA: {0xA5, true}, 0xFE03: {0xA5, true},
	0xFFE7: {0x5B, true}, 0xFFE8: {0x5C, true}, // Meta (Option на Mac) → Win
	0xFFEB: {0x5B, true}, 0xFFEC: {0x5C, true},
	// цифровий блок
	0xFF8D: {0x0D, true}, 0xFF80: {0x20, false}, 0xFF89: {0x09, false},
	0xFFAA: {0x6A, false}, 0xFFAB: {0x6B, false}, 0xFFAC: {0x6C, false},
	0xFFAD: {0x6D, false}, 0xFFAE: {0x6E, false}, 0xFFAF: {0x6F, true},
	0xFF95: {0x24, false}, 0xFF96: {0x25, false}, 0xFF97: {0x26, false},
	0xFF98: {0x27, false}, 0xFF99: {0x28, false}, 0xFF9A: {0x21, false},
	0xFF9B: {0x22, false}, 0xFF9C: {0x23, false}, 0xFF9D: {0x0C, false},
	0xFF9E: {0x2D, false}, 0xFF9F: {0x2E, false},
}

// Кирилиця X11 (keysym 0x6a1..0x6ff) → Unicode.
var (
	cyrLowA = []rune("ђѓёєѕіїјљњћќґўџ")                  // 0x6a1..0x6af
	cyrUpA  = []rune("ЂЃЁЄЅІЇЈЉЊЋЌҐЎЏ")                  // 0x6b1..0x6bf
	cyrLowB = []rune("юабцдефгхийклмнопярстужвьызшэщчъ") // 0x6c0..0x6df
	cyrUpB  = []rune("ЮАБЦДЕФГХИЙКЛМНОПЯРСТУЖВЬЫЗШЭЩЧЪ") // 0x6e0..0x6ff
)

func keysymRune(sym uint32) rune {
	switch {
	case sym >= 0x20 && sym <= 0x7e, sym >= 0xa0 && sym <= 0xff:
		return rune(sym)
	case sym >= 0x01000100 && sym <= 0x0110ffff:
		return rune(sym - 0x01000000)
	case sym >= 0x6a1 && sym <= 0x6af:
		return cyrLowA[sym-0x6a1]
	case sym == 0x6b0:
		return '№'
	case sym >= 0x6b1 && sym <= 0x6bf:
		return cyrUpA[sym-0x6b1]
	case sym >= 0x6c0 && sym <= 0x6df:
		return cyrLowB[sym-0x6c0]
	case sym >= 0x6e0 && sym <= 0x6ff:
		return cyrUpB[sym-0x6e0]
	case sym == 0x20ac:
		return '€'
	}
	return 0
}

func scanOf(vk uint16) uint16 {
	r, _, _ := pMapVirtualKeyW.Call(uintptr(vk), 0)
	return uint16(r)
}

func (st *inputState) held(syms ...uint32) bool {
	for _, s := range syms {
		if _, ok := st.keys[s]; ok {
			return true
		}
	}
	return false
}

// vkForRune шукає клавішу для символу в розкладці hkl; ok=false, якщо немає.
func vkForRune(r rune, hkl uintptr) (vk uint16, shift byte, ok bool) {
	if r > 0xffff {
		return 0, 0, false
	}
	res, _, _ := pVkKeyScanExW.Call(uintptr(r), hkl)
	v := int16(res)
	if v == -1 {
		return 0, 0, false
	}
	return uint16(v) & 0xff, byte(uint16(v) >> 8), true
}

func foregroundLayout() uintptr {
	hwnd, _, _ := pGetForegroundWindow.Call()
	tid, _, _ := pGetWindowThreadProcessId.Call(hwnd, 0)
	hkl, _, _ := pGetKeyboardLayout.Call(tid)
	return hkl
}

func (st *inputState) resolve(sym uint32) (k keyDown, ok bool) {
	if d, found := specialKeys[sym]; found {
		return keyDown{vk: d.vk, scan: scanOf(d.vk), ext: d.ext}, true
	}
	if sym >= 0xFFBE && sym <= 0xFFD5 { // F1..F24
		vk := uint16(0x70 + sym - 0xFFBE)
		return keyDown{vk: vk, scan: scanOf(vk)}, true
	}
	if sym >= 0xFFB0 && sym <= 0xFFB9 { // KP_0..KP_9
		vk := uint16(0x60 + sym - 0xFFB0)
		return keyDown{vk: vk, scan: scanOf(vk)}, true
	}
	r := keysymRune(sym)
	if r == 0 {
		return k, false
	}

	shiftHeld := st.held(0xFFE1, 0xFFE2)
	modHeld := st.held(0xFFE3, 0xFFE4, 0xFFE9, 0xFFEA, 0xFFE7, 0xFFE8, 0xFFEB, 0xFFEC)
	hkl := foregroundLayout()
	vk, shift, found := vkForRune(r, hkl)

	if modHeld { // комбінації (Ctrl+C тощо) потребують справжньої клавіші
		if !found {
			var list [16]uintptr
			n, _, _ := pGetKeyboardLayoutList.Call(16, uintptr(unsafe.Pointer(&list[0])))
			for i := 0; i < int(n) && !found; i++ {
				vk, _, found = vkForRune(r, list[i])
			}
		}
		if !found {
			switch {
			case r >= 'a' && r <= 'z':
				vk, found = uint16(r-'a'+'A'), true
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				vk, found = uint16(r), true
			}
		}
		if found {
			return keyDown{vk: vk, scan: scanOf(vk)}, true
		}
	} else if found && shift&6 == 0 && (shift&1 != 0) == shiftHeld {
		caps, _, _ := pGetKeyState.Call(0x14)
		letter := vk >= 'A' && vk <= 'Z'
		if !(letter && caps&1 != 0) {
			return keyDown{vk: vk, scan: scanOf(vk)}, true
		}
	}

	// символу немає в поточній розкладці — вводимо як Unicode
	if r > 0xffff {
		r -= 0x10000
		k.uni = [2]uint16{0xd800 + uint16(r>>10), 0xdc00 + uint16(r&0x3ff)}
		k.nUni = 2
	} else {
		k.uni[0], k.nUni = uint16(r), 1
	}
	return k, true
}

func emit(k keyDown, down bool) {
	var fl uint32
	if !down {
		fl = keyUp
	}
	if k.nUni > 0 {
		for i := 0; i < k.nUni; i++ {
			sendKey(&keybdInput{scan: k.uni[i], flags: fl | keyUnicode})
		}
		return
	}
	if k.ext {
		fl |= keyExtended
	}
	sendKey(&keybdInput{vk: k.vk, scan: k.scan, flags: fl})
}

func (st *inputState) key(sym uint32, down bool) {
	if st.keys == nil {
		st.keys = map[uint32]keyDown{}
	}
	k, pressed := st.keys[sym]
	if !down {
		if pressed {
			delete(st.keys, sym)
			emit(k, false)
		}
		return
	}
	if !pressed {
		var ok bool
		if k, ok = st.resolve(sym); !ok {
			return
		}
		st.keys[sym] = k
	}
	emit(k, true) // повторне натискання = автоповтор
}

// pointer: x, y — у координатах кадру клієнта (fbW x fbH), який може бути
// зменшеною копією екрана; абсолютні координати Windows від масштабу не залежать.
func (st *inputState) pointer(x, y int, mask byte, fbW, fbH int) {
	in := mouseInput{flags: mouseMove | mouseAbsolute | mouseVirtualDesk}
	if fbW > 1 {
		in.dx = int32((min(x, fbW-1)*65535 + (fbW-1)/2) / (fbW - 1))
	}
	if fbH > 1 {
		in.dy = int32((min(y, fbH-1)*65535 + (fbH-1)/2) / (fbH - 1))
	}
	ch := mask ^ st.buttons
	st.buttons = mask
	btn := func(bit byte, dn, up uint32) {
		if ch&bit != 0 {
			if mask&bit != 0 {
				in.flags |= dn
			} else {
				in.flags |= up
			}
		}
	}
	btn(1, mouseLeftDown, mouseLeftUp)
	btn(2, mouseMiddleDown, mouseMiddleUp)
	btn(4, mouseRightDown, mouseRightUp)
	sendMouse(&in)

	wheel := func(bit byte, flag uint32, delta int32) {
		if ch&bit != 0 && mask&bit != 0 {
			sendMouse(&mouseInput{flags: flag, mouseData: uint32(delta)})
		}
	}
	wheel(8, mouseWheel, 120)
	wheel(16, mouseWheel, -120)
	wheel(32, mouseHWheel, -120)
	wheel(64, mouseHWheel, 120)
}

// releaseAll відпускає все, що клієнт лишив натиснутим.
func (st *inputState) releaseAll() {
	for sym, k := range st.keys {
		emit(k, false)
		delete(st.keys, sym)
	}
	var fl uint32
	if st.buttons&1 != 0 {
		fl |= mouseLeftUp
	}
	if st.buttons&2 != 0 {
		fl |= mouseMiddleUp
	}
	if st.buttons&4 != 0 {
		fl |= mouseRightUp
	}
	if fl != 0 {
		sendMouse(&mouseInput{flags: fl})
	}
	st.buttons = 0
}
