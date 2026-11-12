//go:build windows

package main

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Windows показує вікна дозволу UAC та екран блокування на окремому «захищеному»
// робочому столі. Щоб бачити його й керувати ним, потік має перейти на активний
// робочий стіл вводу — це дозволено лише процесу з правами SYSTEM (режим служби).
// У звичайному режимі виклики нижче просто нічого не змінюють.

var (
	pOpenInputDesktop          = user32.NewProc("OpenInputDesktop")
	pSetThreadDesktop          = user32.NewProc("SetThreadDesktop")
	pCloseDesktop              = user32.NewProc("CloseDesktop")
	pGetUserObjectInformationW = user32.NewProc("GetUserObjectInformationW")
)

// deskFollower — стан одного ОС-потоку (потік має бути закріплений LockOSThread).
type deskFollower struct {
	name    string
	h       uintptr
	checked time.Time
}

// follow переводить потік на активний робочий стіл вводу, якщо той змінився.
// before викликається перед перемиканням: потік не може змінити стіл, поки
// тримає на старому свої об'єкти. Повертає true, якщо стіл змінено.
func (d *deskFollower) follow(before func()) bool {
	d.checked = time.Now()
	h, _, _ := pOpenInputDesktop.Call(0, 0, 0x02000000) // MAXIMUM_ALLOWED
	if h == 0 {
		return false // немає доступу (звичайний режим під час UAC) — лишаємося де є
	}
	var buf [128]uint16
	var n uint32
	pGetUserObjectInformationW.Call(h, 2, uintptr(unsafe.Pointer(&buf[0])), unsafe.Sizeof(buf), uintptr(unsafe.Pointer(&n)))
	name := syscall.UTF16ToString(buf[:])
	if name == d.name || name == "" {
		pCloseDesktop.Call(h)
		return false
	}
	if before != nil {
		before()
	}
	if r, _, _ := pSetThreadDesktop.Call(h); r == 0 {
		pCloseDesktop.Call(h)
		return false
	}
	if d.h != 0 {
		pCloseDesktop.Call(d.h)
	}
	first := d.name == ""
	d.h, d.name = h, name
	if !first {
		logf("активний робочий стіл: %s", name)
	}
	return !first
}

// Увесь ввід (миша, клавіатура) виконує один закріплений потік: SendInput діє на
// робочий стіл потоку, який його викликає, тож цей потік стежить за столом вводу.
var inputJobs = make(chan func(), 64)

func inputLoop() {
	runtime.LockOSThread()
	var d deskFollower
	for job := range inputJobs {
		if time.Since(d.checked) > 100*time.Millisecond {
			d.follow(nil)
		}
		job()
	}
}

// inputDo виконує f у потоці вводу й чекає завершення (порядок подій зберігається).
func inputDo(f func()) {
	done := make(chan struct{})
	inputJobs <- func() { f(); close(done) }
	<-done
}
