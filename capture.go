//go:build windows

package main

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const tile = 32 // розмір клітинки для пошуку змін

// gdiCap захоплює весь віртуальний екран у DIB-секцію (BGRX, згори вниз).
type gdiCap struct {
	vx, vy, w, h int
	screenDC     uintptr
	memDC        uintptr
	bmp, oldBmp  uintptr
	bits         []byte
	hotCursor    uintptr
	hotX, hotY   int32
}

func newGdiCap() *gdiCap {
	c := &gdiCap{}
	c.vx, c.vy, c.w, c.h = captureArea()
	c.screenDC, _, _ = pGetDC.Call(0)
	c.memDC, _, _ = pCreateCompatibleDC.Call(c.screenDC)
	bi := bitmapInfoHeader{
		size: 40, width: int32(c.w), height: -int32(c.h),
		planes: 1, bitCount: 32,
	}
	var ptr uintptr
	c.bmp, _, _ = pCreateDIBSection.Call(c.memDC, uintptr(unsafe.Pointer(&bi)), 0,
		uintptr(unsafe.Pointer(&ptr)), 0, 0)
	if c.bmp == 0 || ptr == 0 {
		c.close()
		return nil
	}
	c.oldBmp, _, _ = pSelectObject.Call(c.memDC, c.bmp)
	c.bits = unsafe.Slice((*byte)(unsafe.Pointer(ptr)), c.w*c.h*4)
	return c
}

func (c *gdiCap) close() {
	if c.memDC != 0 {
		if c.oldBmp != 0 {
			pSelectObject.Call(c.memDC, c.oldBmp)
		}
		pDeleteDC.Call(c.memDC)
	}
	if c.bmp != 0 {
		pDeleteObject.Call(c.bmp)
	}
	if c.screenDC != 0 {
		pReleaseDC.Call(0, c.screenDC)
	}
	c.bits = nil
}

// blit копіює екран у c.bits через GDI і домальовує курсор.
func (c *gdiCap) blit() bool {
	r, _, _ := pBitBlt.Call(c.memDC, 0, 0, uintptr(c.w), uintptr(c.h),
		c.screenDC, uintptr(c.vx), uintptr(c.vy), srcCopy)
	if r == 0 {
		return false
	}
	if cursorInFrame.Load() {
		c.drawCursor()
	}
	return true
}

func (c *gdiCap) drawCursor() {
	ci := cursorInfo{size: uint32(unsafe.Sizeof(cursorInfo{}))}
	r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 || ci.flags&cursorShowing == 0 || ci.hCursor == 0 {
		return
	}
	if ci.hCursor != c.hotCursor {
		var ii iconInfo
		if r, _, _ := pGetIconInfo.Call(ci.hCursor, uintptr(unsafe.Pointer(&ii))); r != 0 {
			c.hotCursor, c.hotX, c.hotY = ci.hCursor, int32(ii.xHot), int32(ii.yHot)
			if ii.hbmMask != 0 {
				pDeleteObject.Call(ii.hbmMask)
			}
			if ii.hbmColor != 0 {
				pDeleteObject.Call(ii.hbmColor)
			}
		}
	}
	x := ci.x - int32(c.vx) - c.hotX
	y := ci.y - int32(c.vy) - c.hotY
	pDrawIconEx.Call(c.memDC, uintptr(x), uintptr(y), ci.hCursor, 0, 0, 0, 0, diNormal)
}

// Screen — спільний кадр і список клієнтів.
type Screen struct {
	mu      sync.RWMutex
	w, h    int
	tw, th  int
	vx, vy  int
	fb      []byte // BGRX, актуальний кадр
	clients map[*Client]struct{}

	cur       *cursorShape // форма курсора для клієнтів, що малюють його самі
	curSerial uint32

	scale    atomic.Int32 // відсоток розміру кадру (100 — без зменшення)
	fbW, fbH atomic.Int32 // поточний розмір кадру для клієнтів

	wake      chan struct{}
	stale     atomic.Bool  // є клієнт, що ще не бачив свіжого захоплення
	lastInput atomic.Int64 // UnixNano останньої події вводу
}

func newScreen() *Screen {
	s := &Screen{clients: map[*Client]struct{}{}, wake: make(chan struct{}, 1)}
	s.scale.Store(100)
	cursorInFrame.Store(true)
	s.vx, s.vy, s.w, s.h = captureArea()
	s.resize(s.w, s.h)
	return s
}

func (s *Screen) resize(w, h int) {
	s.w, s.h = w, h
	s.fbW.Store(int32(w))
	s.fbH.Store(int32(h))
	s.tw, s.th = (w+tile-1)/tile, (h+tile-1)/tile
	s.fb = make([]byte, w*h*4)
}

func (s *Screen) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Screen) add(c *Client) {
	s.mu.Lock()
	c.mu.Lock()
	c.tw, c.th = s.tw, s.th
	c.dirty = make([]bool, s.tw*s.th)
	c.mu.Unlock()
	s.clients[c] = struct{}{}
	s.stale.Store(true)
	s.mu.Unlock()
	s.refreshCursorMode()
	s.poke()
}

func (s *Screen) remove(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	s.refreshCursorMode()
}

func (s *Screen) anyWaiting() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for c := range s.clients {
		c.mu.Lock()
		w := c.want
		c.mu.Unlock()
		if w {
			return true
		}
	}
	return false
}

// capturer — джерело кадрів: DXGI (швидко) або GDI (сумісно).
type capturer interface {
	grab(wait time.Duration) (changed, ok bool)
	frame() []byte
	valid() bool // у frame() вже є справжній кадр
	geom() (vx, vy, w, h int)
	fast() bool
	close()
}

func (c *gdiCap) grab(time.Duration) (bool, bool) { return true, c.blit() }
func (c *gdiCap) frame() []byte                   { return c.bits }
func (c *gdiCap) geom() (int, int, int, int)      { return c.vx, c.vy, c.w, c.h }
func (c *gdiCap) fast() bool                      { return false }
func (c *gdiCap) valid() bool                     { return true }
func (c *dxgiCap) valid() bool                    { return c.ready }
func (c *dxgiCap) fast() bool                     { return true }

// openCapturer пробує DXGI, а якщо він недоступний — GDI.
func openCapturer() capturer {
	if !noDXGI {
		if d := newDxgiCap(); d != nil {
			return d
		}
	}
	if g := newGdiCap(); g != nil {
		return g
	}
	return nil
}

// run — цикл захоплення; працює лише коли хтось чекає на оновлення.
func (s *Screen) run() {
	runtime.LockOSThread()
	var cap capturer
	var changed []bool
	var scaled []byte
	var scaledFrom [2]int
	var xmap []int32
	var curTrack cursorTrack
	var desk deskFollower
	var kind string
	var retryDX time.Time
	retryEvery := 10 * time.Second
	idleSince := time.Now()
	var statFrames int
	var statGrab, statDiff time.Duration
	statAt := time.Now()
	drop := func() {
		if cap != nil {
			cap.close()
			cap = nil
		}
	}
	for {
		if !s.anyWaiting() {
			select {
			case <-s.wake:
			case <-time.After(2 * time.Second):
				s.mu.RLock()
				n := len(s.clients)
				s.mu.RUnlock()
				if n == 0 { // нікого немає — звільняємо ресурси
					drop()
				}
			}
			continue
		}
		start := time.Now()

		// у режимі служби переходимо на захищений робочий стіл (UAC, екран входу)
		if start.Sub(desk.checked) > 250*time.Millisecond {
			desk.follow(drop)
		}

		vx, vy, w, h := captureArea()
		if cap != nil {
			if cx, cy, cw, ch := cap.geom(); cx != vx || cy != vy || cw != w || ch != h {
				drop()
			}
		}
		if cap != nil && !cap.fast() && !noDXGI && start.After(retryDX) {
			drop() // періодично пробуємо повернутися з GDI на DXGI
		}
		if cap == nil {
			if cap = openCapturer(); cap == nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			k := "GDI"
			if cap.fast() {
				k, retryEvery = "DXGI", 10*time.Second
			} else if retryEvery < 5*time.Minute {
				retryEvery *= 2 // DXGI недоступний — пробуємо дедалі рідше
			}
			retryDX = start.Add(retryEvery)
			if k != kind {
				kind = k
				logf("захоплення екрана: %s", kind)
			}
		}

		got, ok := cap.grab(100 * time.Millisecond)
		tGrab := time.Since(start)
		if !ok {
			// робочий стіл недоступний (UAC, екран блокування, зміна режиму)
			drop()
			if !desk.follow(nil) {
				time.Sleep(200 * time.Millisecond)
			}
			continue
		}
		s.pollCursor(&curTrack)
		if !got && (!s.stale.Load() || !cap.valid()) {
			continue // екран не змінювався; grab уже зачекав
		}
		bits := cap.frame()
		cvx, cvy, cw, ch := cap.geom()

		// масштаб: зменшуємо кадр до порівняння, тож клієнт отримує менше даних
		if sc := int(s.scale.Load()); sc > 0 && sc < 100 {
			dw, dh := max(cw*sc/100, 64), max(ch*sc/100, 64) // та сама формула, що у frameSize
			if len(scaled) != dw*dh*4 || scaledFrom != [2]int{cw, ch} {
				scaled = make([]byte, dw*dh*4)
				scaledFrom = [2]int{cw, ch}
				xmap = make([]int32, dw)
				for x := range xmap {
					xmap[x] = int32(min(x*cw/dw, cw-2))
				}
			}
			downscale(scaled, dw, dh, bits, cw, ch, xmap)
			bits, cw, ch = scaled, dw, dh
		}

		if cw != s.w || ch != s.h {
			s.mu.Lock()
			s.resize(cw, ch)
			s.vx, s.vy = cvx, cvy
			copy(s.fb, bits)
			for c := range s.clients {
				c.mu.Lock()
				c.tw, c.th = s.tw, s.th
				c.dirty = make([]bool, s.tw*s.th)
				for i := range c.dirty {
					c.dirty[i] = true
				}
				c.anyDirty, c.resized, c.fresh = true, true, true
				c.mu.Unlock()
				c.notify()
			}
			s.mu.Unlock()
			idleSince = time.Now()
			continue
		}
		s.vx, s.vy = cvx, cvy

		if len(changed) != s.tw*s.th {
			changed = make([]bool, s.tw*s.th)
		}
		any := s.diff(bits, changed)
		if verbose {
			statFrames++
			statGrab += tGrab
			statDiff += time.Since(start) - tGrab
			if time.Since(statAt) > 3*time.Second {
				logf("%s: %d кадрів, захоплення %v, порівняння %v на кадр", kind, statFrames,
					(statGrab / time.Duration(statFrames)).Round(100*time.Microsecond),
					(statDiff / time.Duration(statFrames)).Round(100*time.Microsecond))
				statFrames, statGrab, statDiff, statAt = 0, 0, 0, time.Now()
			}
		}
		if any || s.stale.Load() {
			s.publish(bits, changed, any)
		}
		if any {
			idleSince = time.Now()
		}

		// темп: DXGI сам чекає на зміни — лише обмежуємо до ~60 к/с;
		// GDI опитуємо ~30 к/с під час активності й рідше, коли екран нерухомий
		interval := 16 * time.Millisecond
		if !cap.fast() {
			interval = 33 * time.Millisecond
			now := time.Now()
			quiet := now.Sub(idleSince)
			if in := time.Duration(now.UnixNano() - s.lastInput.Load()); in < quiet {
				quiet = in
			}
			if quiet > 3*time.Second {
				interval = 150 * time.Millisecond
			} else if quiet > time.Second {
				interval = 66 * time.Millisecond
			}
		}
		if d := interval - time.Since(start); d > 0 {
			if cap.fast() {
				time.Sleep(d)
			} else {
				select {
				case <-time.After(d):
				case <-s.wake:
				}
			}
		}
	}
}

// frameSize — розмір кадру, який бачать клієнти за поточного масштабу.
func (s *Screen) frameSize() (int, int) {
	_, _, w, h := captureArea()
	if sc := int(s.scale.Load()); sc > 0 && sc < 100 {
		w, h = max(w*sc/100, 64), max(h*sc/100, 64)
	}
	return w, h
}

// scaleSteps — дозволені масштаби у відсотках.
var scaleSteps = []int{100, 75, 66, 50, 33, 25}

// setScale змінює масштаб на льоту; клієнти отримують новий розмір екрана.
func (s *Screen) setScale(p int) bool {
	for _, v := range scaleSteps {
		if v == p {
			s.scale.Store(int32(p))
			s.stale.Store(true) // змусити цикл захоплення перевипустити кадр
			s.poke()
			return true
		}
	}
	return false
}

// downscale зменшує кадр: кожен піксель — середнє блоку 2x2 у вихідній точці.
// Для 50% це точне усереднення, для інших масштабів — добрий компроміс зі швидкістю.
func downscale(dst []byte, dw, dh int, src []byte, sw, sh int, xmap []int32) {
	const m = 0x00ff00ff
	out := unsafe.Slice((*uint32)(unsafe.Pointer(&dst[0])), dw*dh)
	in := unsafe.Slice((*uint32)(unsafe.Pointer(&src[0])), sw*sh)
	for y := 0; y < dh; y++ {
		sy := min(y*sh/dh, sh-2)
		r0 := in[sy*sw : sy*sw+sw]
		r1 := in[(sy+1)*sw : (sy+1)*sw+sw]
		d := out[y*dw : y*dw+dw]
		for x, sx := range xmap {
			a, b, c, e := r0[sx], r0[sx+1], r1[sx], r1[sx+1]
			rb := ((a & m) + (b & m) + (c & m) + (e & m) + 0x00020002) >> 2 & m
			g := ((a >> 8 & 0xff) + (b >> 8 & 0xff) + (c >> 8 & 0xff) + (e >> 8 & 0xff) + 2) >> 2
			d[x] = 0xff000000 | rb | g<<8
		}
	}
}

// diff порівнює новий кадр із поточним поклітинно. Читає s.fb без блокування:
// пише в нього лише цей самий потік.
func (s *Screen) diff(nb []byte, changed []bool) bool {
	any := false
	stride := s.w * 4
	for ty := 0; ty < s.th; ty++ {
		y0 := ty * tile
		y1 := y0 + tile
		if y1 > s.h {
			y1 = s.h
		}
		row := changed[ty*s.tw : (ty+1)*s.tw]
		for i := range row {
			row[i] = false
		}
		for y := y0; y < y1; y++ {
			off := y * stride
			if bytes.Equal(nb[off:off+stride], s.fb[off:off+stride]) {
				continue
			}
			for tx := 0; tx < s.tw; tx++ {
				if row[tx] {
					continue
				}
				a := off + tx*tile*4
				b := a + tile*4
				if b > off+stride {
					b = off + stride
				}
				if !bytes.Equal(nb[a:b], s.fb[a:b]) {
					row[tx] = true
					any = true
				}
			}
		}
	}
	return any
}

// publish переносить змінені клітинки в s.fb і позначає їх усім клієнтам.
// Також позначає клієнтів «свіжими»: новий клієнт не отримує кадр, доки після
// його підключення не відбулося хоча б одне захоплення.
func (s *Screen) publish(nb []byte, changed []bool, any bool) {
	stride := s.w * 4
	s.mu.Lock()
	s.stale.Store(false)
	for ty := 0; any && ty < s.th; ty++ {
		y0 := ty * tile
		y1 := y0 + tile
		if y1 > s.h {
			y1 = s.h
		}
		for tx := 0; tx < s.tw; {
			if !changed[ty*s.tw+tx] {
				tx++
				continue
			}
			x0 := tx
			for tx < s.tw && changed[ty*s.tw+tx] {
				tx++
			}
			a := x0 * tile * 4
			b := tx * tile * 4
			if b > stride {
				b = stride
			}
			for y := y0; y < y1; y++ {
				copy(s.fb[y*stride+a:y*stride+b], nb[y*stride+a:y*stride+b])
			}
		}
	}
	for c := range s.clients {
		c.mu.Lock()
		if any && len(c.dirty) == len(changed) {
			for i, ch := range changed {
				if ch {
					c.dirty[i] = true
				}
			}
			c.anyDirty = true
		}
		c.fresh = true
		c.mu.Unlock()
		c.notify()
	}
	s.mu.Unlock()
}
