//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	pGetDC                         = user32.NewProc("GetDC")
	pReleaseDC                     = user32.NewProc("ReleaseDC")
	pSendInput                     = user32.NewProc("SendInput")
	pGetCursorInfo                 = user32.NewProc("GetCursorInfo")
	pGetIconInfo                   = user32.NewProc("GetIconInfo")
	pDrawIconEx                    = user32.NewProc("DrawIconEx")
	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	pVkKeyScanExW                  = user32.NewProc("VkKeyScanExW")
	pMapVirtualKeyW                = user32.NewProc("MapVirtualKeyW")
	pGetKeyboardLayout             = user32.NewProc("GetKeyboardLayout")
	pGetKeyboardLayoutList         = user32.NewProc("GetKeyboardLayoutList")
	pGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	pGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	pGetKeyState                   = user32.NewProc("GetKeyState")
	pOpenClipboard                 = user32.NewProc("OpenClipboard")
	pCloseClipboard                = user32.NewProc("CloseClipboard")
	pEmptyClipboard                = user32.NewProc("EmptyClipboard")
	pSetClipboardData              = user32.NewProc("SetClipboardData")
	pGetClipboardData              = user32.NewProc("GetClipboardData")
	pGetClipboardSequenceNumber    = user32.NewProc("GetClipboardSequenceNumber")

	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pBitBlt             = gdi32.NewProc("BitBlt")

	pSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	pGlobalAlloc        = kernel32.NewProc("GlobalAlloc")
	pGlobalLock         = kernel32.NewProc("GlobalLock")
	pGlobalUnlock       = kernel32.NewProc("GlobalUnlock")
	pGlobalFree         = kernel32.NewProc("GlobalFree")
	pRtlMoveMemory      = kernel32.NewProc("RtlMoveMemory")
)

const (
	smCxScreen        = 0
	smCyScreen        = 1
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCxVirtualScreen = 78
	smCyVirtualScreen = 79

	srcCopy = 0x00CC0020

	inputMouse    = 0
	inputKeyboard = 1

	mouseMove        = 0x0001
	mouseLeftDown    = 0x0002
	mouseLeftUp      = 0x0004
	mouseRightDown   = 0x0008
	mouseRightUp     = 0x0010
	mouseMiddleDown  = 0x0020
	mouseMiddleUp    = 0x0040
	mouseWheel       = 0x0800
	mouseHWheel      = 0x1000
	mouseVirtualDesk = 0x4000
	mouseAbsolute    = 0x8000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004

	cursorShowing = 0x0001
	diNormal      = 0x0003

	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// INPUT із MOUSEINPUT (amd64: 40 байт).
type mouseInput struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	_         uint32
	extra     uintptr
}

// INPUT із KEYBDINPUT (amd64: 40 байт).
type keybdInput struct {
	typ      uint32
	_        uint32
	vk, scan uint16
	flags    uint32
	time     uint32
	_        uint32
	extra    uintptr
	_        [8]byte
}

type cursorInfo struct {
	size    uint32
	flags   uint32
	hCursor uintptr
	x, y    int32
}

type iconInfo struct {
	fIcon    int32
	xHot     uint32
	yHot     uint32
	_        uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bitmapInfoHeader struct {
	size          uint32
	width         int32
	height        int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xPelsPerMeter int32
	yPelsPerMeter int32
	clrUsed       uint32
	clrImportant  uint32
}

func metric(i int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

// virtualScreen повертає прямокутник усіх моніторів.
func virtualScreen() (x, y, w, h int) {
	x, y = metric(smXVirtualScreen), metric(smYVirtualScreen)
	w, h = metric(smCxVirtualScreen), metric(smCyVirtualScreen)
	if w <= 0 || h <= 0 {
		x, y, w, h = 0, 0, metric(smCxScreen), metric(smCyScreen)
	}
	return
}

func setDPIAware() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	if pSetProcessDpiAwarenessContext.Find() == nil {
		if r, _, _ := pSetProcessDpiAwarenessContext.Call(^uintptr(3)); r != 0 {
			return
		}
	}
	pSetProcessDPIAware.Call()
}

func sendMouse(in *mouseInput) {
	in.typ = inputMouse
	pSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
}

func sendKey(in *keybdInput) {
	in.typ = inputKeyboard
	pSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
}
