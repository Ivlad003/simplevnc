//go:build windows

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Файли в буфері обміну Windows (формат CF_HDROP — те, що кладе Провідник, коли
// файли копіюють через Ctrl+C).

const cfHDrop = 15

var pDragQueryFileW = syscall.NewLazyDLL("shell32.dll").NewProc("DragQueryFileW")

// getClipboardFiles повертає шляхи файлів і тек, які зараз лежать у буфері обміну.
func getClipboardFiles() []string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !openClipboard() {
		return nil
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfHDrop)
	if h == 0 {
		return nil
	}
	n, _, _ := pDragQueryFileW.Call(h, 0xFFFFFFFF, 0, 0)
	var out []string
	for i := uintptr(0); i < n && i < 4096; i++ {
		l, _, _ := pDragQueryFileW.Call(h, i, 0, 0)
		if l == 0 {
			continue
		}
		buf := make([]uint16, l+1)
		pDragQueryFileW.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), l+1)
		out = append(out, syscall.UTF16ToString(buf))
	}
	return out
}

// setClipboardFiles кладе файли в буфер обміну: після цього Ctrl+V у Провіднику
// вставляє їх, як після звичайного копіювання.
func setClipboardFiles(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	// DROPFILES: зміщення списку, точка, прапорці; далі шляхи UTF-16 через нуль,
	// наприкінці ще один нуль.
	buf := make([]byte, 20)
	binary.LittleEndian.PutUint32(buf[0:], 20)
	binary.LittleEndian.PutUint32(buf[16:], 1) // fWide
	for _, p := range paths {
		for _, c := range utf16.Encode([]rune(p)) {
			buf = binary.LittleEndian.AppendUint16(buf, c)
		}
		buf = append(buf, 0, 0)
	}
	buf = append(buf, 0, 0)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !openClipboard() {
		return false
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(buf)))
	if h == 0 {
		return false
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return false
	}
	pRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(cfHDrop, h); r == 0 {
		pGlobalFree.Call(h)
		return false
	}
	seq, _, _ := pGetClipboardSequenceNumber.Call()
	ownClipSeq.Store(uint32(seq))
	return true
}

// clipDirOverride — тека для отриманих файлів, задана параметром -clipdir.
var clipDirOverride string

// receivedDir — куди зберігати файли, надіслані на ПК: «Завантаження\SimpleVNC»
// користувача, який зараз працює за комп'ютером. Служба виконується від імені
// системи, тож теку користувача дізнаємося через його сеанс.
func receivedDir() string {
	if clipDirOverride != "" {
		return clipDirOverride
	}
	home := ""
	if serviceMode {
		var sid uint32
		if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid) == nil {
			var tok windows.Token
			if windows.WTSQueryUserToken(sid, &tok) == nil {
				home, _ = tok.GetUserProfileDirectory()
				tok.Close()
			}
		}
		if home == "" { // ніхто не ввійшов у систему
			return filepath.Join(os.Getenv("PUBLIC"), "Downloads", "SimpleVNC")
		}
	} else {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, "Downloads", "SimpleVNC")
}
