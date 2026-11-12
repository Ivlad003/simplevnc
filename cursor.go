//go:build windows

package main

import (
	"sync/atomic"
	"unsafe"
)

// Курсор на боці клієнта (псевдокодування Cursor, -239): сервер шле лише форму
// курсора, а малює його сам клієнт у місці свого вказівника — без затримки мережі.

const encCursor = -239

var pGetObjectW = gdi32.NewProc("GetObjectW")

// cursorInFrame — курсор домальовується в кадр: є клієнт, який не вміє малювати
// його сам, або це ввімкнено примусово.
var (
	cursorInFrame  atomic.Bool
	forceDrawnCurs bool
)

type cursorShape struct {
	w, h       int
	hotX, hotY int
	bgra       []byte // w*h*4, колір без попереднього множення на альфу
	alpha      []byte // w*h
}

type bitmapObj struct {
	typ, width, height, widthBytes int32
	planes, bitsPixel              uint16
	bits                           uintptr
}

// grabCursorShape знімає форму курсора: малює його на чорному й білому тлі та з
// різниці відновлює колір і прозорість. Так однаково обробляються кольорові,
// монохромні й «інверсні» курсори.
func grabCursorShape(hCursor uintptr) *cursorShape {
	var ii iconInfo
	if r, _, _ := pGetIconInfo.Call(hCursor, uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil
	}
	defer func() {
		if ii.hbmMask != 0 {
			pDeleteObject.Call(ii.hbmMask)
		}
		if ii.hbmColor != 0 {
			pDeleteObject.Call(ii.hbmColor)
		}
	}()
	hb := ii.hbmColor
	if hb == 0 {
		hb = ii.hbmMask
	}
	var bm bitmapObj
	if r, _, _ := pGetObjectW.Call(hb, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil
	}
	w, h := int(bm.width), int(bm.height)
	if ii.hbmColor == 0 {
		h /= 2 // монохромний: маска AND над маскою XOR
	}
	if w <= 0 || h <= 0 || w > 256 || h > 256 {
		return nil
	}

	screenDC, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screenDC)
	memDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	defer pDeleteDC.Call(memDC)
	bi := bitmapInfoHeader{size: 40, width: int32(w), height: -int32(h), planes: 1, bitCount: 32}
	var ptr uintptr
	bmp, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&ptr)), 0, 0)
	if bmp == 0 || ptr == 0 {
		return nil
	}
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(memDC, bmp)
	defer pSelectObject.Call(memDC, old)
	px := unsafe.Slice((*uint32)(unsafe.Pointer(ptr)), w*h)

	draw := func(bg uint32) []uint32 {
		for i := range px {
			px[i] = bg
		}
		pDrawIconEx.Call(memDC, 0, 0, hCursor, uintptr(w), uintptr(h), 0, 0, diNormal)
		return append([]uint32(nil), px...)
	}
	black, white := draw(0), draw(0x00ffffff)

	sh := &cursorShape{w: w, h: h, hotX: int(ii.xHot), hotY: int(ii.yHot),
		bgra: make([]byte, w*h*4), alpha: make([]byte, w*h)}
	inverted := make([]bool, w*h)
	for i := range black {
		var maxDiff, minDiff = -255, 255
		var bc [3]int
		for k := 0; k < 3; k++ {
			b, wv := int(black[i]>>(8*k))&0xff, int(white[i]>>(8*k))&0xff
			bc[k] = b
			maxDiff, minDiff = max(maxDiff, wv-b), min(minDiff, wv-b)
		}
		if minDiff < -64 { // на чорному світліше, ніж на білому: піксель інвертує тло
			inverted[i] = true
			sh.alpha[i] = 255 // малюємо чорним, контур додамо нижче
			continue
		}
		a := min(max(255-maxDiff, 0), 255)
		sh.alpha[i] = byte(a)
		if a > 0 {
			for k := 0; k < 3; k++ {
				sh.bgra[i*4+k] = byte(min(bc[k]*255/a, 255))
			}
		}
	}
	// білий контур навколо інверсних пікселів, щоб курсор було видно на темному
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if sh.alpha[i] >= 128 {
				continue
			}
			near := (x > 0 && inverted[i-1]) || (x < w-1 && inverted[i+1]) ||
				(y > 0 && inverted[i-w]) || (y < h-1 && inverted[i+w])
			if near {
				sh.alpha[i] = 255
				sh.bgra[i*4], sh.bgra[i*4+1], sh.bgra[i*4+2] = 255, 255, 255
			}
		}
	}
	return sh
}

// scaled зменшує форму під масштаб кадру, щоб курсор не здавався завеликим.
func (c *cursorShape) scaled(pct int) *cursorShape {
	if pct >= 100 {
		return c
	}
	w, h := max(c.w*pct/100, 1), max(c.h*pct/100, 1)
	out := &cursorShape{w: w, h: h, hotX: c.hotX * pct / 100, hotY: c.hotY * pct / 100,
		bgra: make([]byte, w*h*4), alpha: make([]byte, w*h)}
	for y := 0; y < h; y++ {
		sy := y * c.h / h
		for x := 0; x < w; x++ {
			si := sy*c.w + x*c.w/w
			copy(out.bgra[(y*w+x)*4:], c.bgra[si*4:si*4+4])
			out.alpha[y*w+x] = c.alpha[si]
		}
	}
	return out
}

// cursorTrack — що вже опубліковано (стан потоку захоплення).
type cursorTrack struct {
	h uintptr
}

// pollCursor перевіряє, чи змінилася форма курсора, і публікує нову клієнтам.
func (s *Screen) pollCursor(t *cursorTrack) {
	ci := cursorInfo{size: uint32(unsafe.Sizeof(cursorInfo{}))}
	r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 || ci.flags&cursorShowing == 0 || ci.hCursor == 0 {
		return
	}
	if ci.hCursor == t.h {
		return
	}
	sh := grabCursorShape(ci.hCursor)
	if sh == nil {
		return
	}
	t.h = ci.hCursor
	s.mu.Lock()
	s.cur = sh
	s.curSerial++
	if s.curSerial < 2 { // 0 — «нічого не надіслано», 1 — прозорий курсор
		s.curSerial = 2
	}
	for _, v := range s.views {
		for c := range v.clients {
			c.notify()
		}
	}
	s.mu.Unlock()
}

// refreshCursorMode вирішує, чи домальовувати курсор у кадр.
func (s *Screen) refreshCursorMode() {
	inFrame := forceDrawnCurs
	clients := s.allClients()
	for _, c := range clients {
		c.mu.Lock()
		if !c.canCursor {
			inFrame = true
		}
		c.mu.Unlock()
	}
	if len(clients) == 0 {
		inFrame = true
	}
	if cursorInFrame.Swap(inFrame) != inFrame {
		s.stale.Store(true) // перемалювати кадр із курсором або без нього
		for _, c := range clients {
			c.notify()
		}
		s.poke()
	}
}

// cursorRect дописує прямокутник псевдокодування Cursor. sh == nil — прозорий
// курсор 1x1 (коли курсор уже намальовано в кадрі).
func (e *encoder) cursorRect(out []byte, sh *cursorShape) []byte {
	w, h, hx, hy := 1, 1, 0, 0
	if sh != nil {
		w, h, hx, hy = sh.w, sh.h, sh.hotX, sh.hotY
	}
	out = append(out, byte(hx>>8), byte(hx), byte(hy>>8), byte(hy), byte(w>>8), byte(w), byte(h>>8), byte(h),
		0xff, 0xff, 0xff, 0x11) // -239
	if sh == nil {
		out = e.appendP(out, 0)
		return append(out, 0)
	}
	for i := 0; i < w*h; i++ {
		out = e.appendP(out, uint32(sh.bgra[i*4])|uint32(sh.bgra[i*4+1])<<8|uint32(sh.bgra[i*4+2])<<16)
	}
	for y := 0; y < h; y++ { // маска: 1 біт на піксель, старший біт перший
		var acc byte
		for x := 0; x < w; x++ {
			if sh.alpha[y*w+x] >= 128 {
				acc |= 0x80 >> (x & 7)
			}
			if x&7 == 7 || x == w-1 {
				out = append(out, acc)
				acc = 0
			}
		}
	}
	return out
}
