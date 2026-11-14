//go:build windows

package main

import "testing"

func TestStuckButtons(t *testing.T) {
	const left, middle, right = 0x01, 0x04, 0x02 // коди кнопок для GetAsyncKeyState
	cases := []struct {
		name       string
		mask, held byte
		down       []uintptr
		want       byte
	}{
		{"нічого не затиснуто", 0, 0, nil, 0},
		{"права залипла, клієнт лише рухає мишу", 0, 0, []uintptr{right}, 4},
		{"права залипла, клієнт тисне ліву", 1, 0, []uintptr{right}, 4},
		{"клієнт сам тримає праву", 4, 4, []uintptr{right}, 0},
		{"клієнт саме відпускає праву — це зробить звичайна подія", 0, 4, []uintptr{right}, 0},
		{"праву тримає інший клієнт", 1, 4, []uintptr{right}, 0},
		{"клієнт тисне ту саму кнопку, що залипла", 4, 0, []uintptr{right}, 0},
		{"залипли всі три", 0, 0, []uintptr{left, middle, right}, 7},
		{"біти колеса кнопок не стосуються", 8, 16, []uintptr{middle}, 2},
	}
	for _, c := range cases {
		down := func(vk uintptr) bool {
			for _, d := range c.down {
				if d == vk {
					return true
				}
			}
			return false
		}
		if got := stuckButtons(c.mask, c.held, down); got != c.want {
			t.Errorf("%s: stuckButtons(%#x, %#x) = %#x, очікував %#x", c.name, c.mask, c.held, got, c.want)
		}
	}
}
