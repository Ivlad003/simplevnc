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

// secure — потік зараз на захищеному робочому столі (UAC, екран входу).
func (d *deskFollower) secure() bool {
	return d.name != "" && d.name != "Default"
}

// Увесь ввід (миша, клавіатура) виконує один закріплений потік: SendInput діє на
// робочий стіл потоку, який його викликає, тож цей потік стежить за столом вводу.
type inputJob struct {
	f       func()
	guarded bool // ввід від агента: на захищеному робочому столі не виконується
	done    chan struct{}
}

var inputJobs = make(chan inputJob, 64)

func inputLoop() {
	runtime.LockOSThread()
	var d deskFollower
	var warned, failLogged time.Time
	failed := 0
	for job := range inputJobs {
		// для агента перевіряємо стіл перед кожною дією, щоб жодна не «проскочила»
		if job.guarded || time.Since(d.checked) > 100*time.Millisecond {
			d.follow(nil)
		}
		if job.guarded && d.secure() {
			if time.Since(warned) > 5*time.Second {
				warned = time.Now()
				logf("ввід агента заблоковано: на екрані вікно дозволу Windows або екран входу")
			}
		} else {
			job.f()
		}
		if inputFail.n > 0 {
			// Найчастіша причина — робочий стіл вводу щойно змінився (вікно дозволу
			// Windows, екран входу): перед наступною подією перевіримо його знову.
			failed += inputFail.n
			inputFail.n = 0
			d.checked = time.Time{}
		}
		if failed > 0 && time.Since(failLogged) > 10*time.Second {
			logf("ввід: Windows не прийняв подій: %d (помилка %d)", failed, uint32(inputFail.err))
			failed, failLogged = 0, time.Now()
		}
		close(job.done)
	}
}

// releaseAllInput відпускає клавіші й кнопки миші, які тримають клієнти. Викликається
// перед завершенням процесу: інакше Windows і далі вважав би їх натиснутими.
func releaseAllInput() {
	job := inputJob{done: make(chan struct{}), f: func() {
		for st := range activeInputs {
			st.releaseAll()
		}
	}}
	select {
	case inputJobs <- job:
		select {
		case <-job.done:
		case <-time.After(time.Second):
		}
	case <-time.After(time.Second):
	}
}

// inputRun виконує f у потоці вводу й чекає завершення (порядок подій зберігається).
func inputRun(f func(), guarded bool) {
	job := inputJob{f: f, guarded: guarded, done: make(chan struct{})}
	inputJobs <- job
	<-job.done
}

func inputDo(f func()) { inputRun(f, false) }
