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

// view — кадр одного масштабу та клієнти, які його отримують. Різним клієнтам
// потрібен різний розмір: рідному VNC-клієнту — повний, браузеру на телефоні — у
// кілька разів менший. Захоплення одне на всіх, зменшення — окреме для кожного view.
type view struct {
	scale   int
	w, h    int
	tw, th  int
	fb      []byte // BGRX, актуальний кадр цього масштабу
	clients map[*Client]struct{}

	// належить потокові захоплення
	scaled  []byte
	from    [2]int
	xmap    []int32
	changed []bool
}

func (v *view) resize(w, h int) {
	v.w, v.h = w, h
	v.tw, v.th = (w+tile-1)/tile, (h+tile-1)/tile
	v.fb = make([]byte, w*h*4)
}

// Screen — джерело кадрів і всі його view.
type Screen struct {
	mu    sync.RWMutex
	views map[int]*view // за масштабом у відсотках

	cur       *cursorShape // форма курсора в повному розмірі; під view її зменшують при відправці
	curSerial uint32

	scale atomic.Int32 // масштаб для клієнтів, які не просили свого (звичайні VNC-клієнти)

	wake      chan struct{}
	stale     atomic.Bool  // є клієнт, що ще не бачив свіжого захоплення
	lastInput atomic.Int64 // UnixNano останньої події вводу
}

func newScreen() *Screen {
	s := &Screen{views: map[int]*view{}, wake: make(chan struct{}, 1)}
	s.scale.Store(100)
	cursorInFrame.Store(true)
	return s
}

func (s *Screen) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// scaledSize — розмір кадру w x h у масштабі scale відсотків.
func scaledSize(w, h, scale int) (int, int) {
	if scale > 0 && scale < 100 {
		return max(w*scale/100, 64), max(h*scale/100, 64)
	}
	return w, h
}

// frameSizeFor — розмір кадру для клієнта з масштабом scale (0 — типовий).
func (s *Screen) frameSizeFor(scale int) (int, int) {
	if scale == 0 {
		scale = int(s.scale.Load())
	}
	_, _, w, h := captureArea()
	return scaledSize(w, h, scale)
}

func (s *Screen) frameSize() (int, int) { return s.frameSizeFor(0) }

// viewLocked повертає view потрібного масштабу, створюючи його за потреби.
// mu має бути взятий на запис.
func (s *Screen) viewLocked(scale int) *view {
	if v := s.views[scale]; v != nil {
		return v
	}
	_, _, aw, ah := captureArea()
	v := &view{scale: scale, clients: map[*Client]struct{}{}}
	v.resize(scaledSize(aw, ah, scale))
	s.views[scale] = v
	return v
}

// attachLocked ставить клієнта у view (mu взятий на запис). Клієнт отримає повний
// кадр; resized — чи треба повідомити йому новий розмір.
func (s *Screen) attachLocked(c *Client, v *view, resized bool) {
	if old := c.v; old != nil && old != v {
		delete(old.clients, c)
		if len(old.clients) == 0 && s.views[old.scale] == old {
			delete(s.views, old.scale)
		}
	}
	c.v = v
	v.clients[c] = struct{}{}
	c.mu.Lock()
	c.tw, c.th = v.tw, v.th
	c.dirty = make([]bool, v.tw*v.th)
	for i := range c.dirty {
		c.dirty[i] = true
	}
	c.spare = nil
	c.anyDirty, c.fresh = true, false
	c.resized = c.resized || resized
	c.curSerial = 0 // курсор під новий масштаб треба надіслати знову
	c.mu.Unlock()
}

// add підключає клієнта; scale — масштаб, який він просив, або 0 для типового.
func (s *Screen) add(c *Client, scale int) {
	c.scaleReq = scale
	if scale == 0 {
		scale = int(s.scale.Load())
	}
	s.mu.Lock()
	s.attachLocked(c, s.viewLocked(scale), false)
	s.mu.Unlock()
	s.stale.Store(true)
	s.refreshCursorMode()
	s.poke()
}

func (s *Screen) remove(c *Client) {
	s.mu.Lock()
	if v := c.v; v != nil {
		delete(v.clients, c)
		if len(v.clients) == 0 && s.views[v.scale] == v {
			delete(s.views, v.scale)
		}
	}
	s.mu.Unlock()
	s.refreshCursorMode()
}

// allClients — усі підключені клієнти.
func (s *Screen) allClients() []*Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Client
	for _, v := range s.views {
		for c := range v.clients {
			out = append(out, c)
		}
	}
	return out
}

func (s *Screen) anyWaiting() bool {
	for _, c := range s.allClients() {
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
				if len(s.allClients()) == 0 { // нікого немає — звільняємо ресурси
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
		force := s.stale.Swap(false) // хтось чекає на перший кадр: публікуємо навіть без змін
		if !got && (!force || !cap.valid()) {
			if force {
				s.stale.Store(true) // кадру ще немає — повторимо наступного разу
			}
			continue // екран не змінювався; grab уже зачекав
		}
		bits := cap.frame()
		_, _, cw, ch := cap.geom()

		s.mu.RLock()
		views := make([]*view, 0, len(s.views))
		for _, v := range s.views {
			if len(v.clients) > 0 {
				views = append(views, v)
			}
		}
		s.mu.RUnlock()

		any := false
		for _, v := range views {
			nb, vw, vh := bits, cw, ch
			if v.scale < 100 { // зменшуємо кадр до порівняння: клієнт отримує менше даних
				vw, vh = scaledSize(cw, ch, v.scale)
				if len(v.scaled) != vw*vh*4 || v.from != [2]int{cw, ch} {
					v.scaled = make([]byte, vw*vh*4)
					v.from = [2]int{cw, ch}
					v.xmap = make([]int32, vw)
					for x := range v.xmap {
						v.xmap[x] = int32(min(x*cw/vw, cw-2))
					}
				}
				downscale(v.scaled, vw, vh, bits, cw, ch, v.xmap)
				nb = v.scaled
			}
			if vw != v.w || vh != v.h {
				s.resizeView(v, vw, vh, nb)
				any = true
				continue
			}
			if len(v.changed) != v.tw*v.th {
				v.changed = make([]bool, v.tw*v.th)
			}
			changed := v.diff(nb)
			if changed || force {
				s.publish(v, nb, changed)
			}
			any = any || changed
		}
		if verbose {
			statFrames++
			statGrab += tGrab
			statDiff += time.Since(start) - tGrab
			if time.Since(statAt) > 3*time.Second {
				logf("%s: %d кадрів, захоплення %v, обробка %v на кадр (розмірів кадру: %d)", kind, statFrames,
					(statGrab / time.Duration(statFrames)).Round(100*time.Microsecond),
					(statDiff / time.Duration(statFrames)).Round(100*time.Microsecond), len(views))
				statFrames, statGrab, statDiff, statAt = 0, 0, 0, time.Now()
			}
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

// scaleSteps — дозволені масштаби у відсотках.
var scaleSteps = []int{100, 75, 66, 50, 33, 25}

func validScale(p int) bool {
	for _, v := range scaleSteps {
		if v == p {
			return true
		}
	}
	return false
}

// setScale змінює типовий масштаб на льоту. Клієнти, які не просили власного
// масштабу, переходять на новий розмір; решта лишаються при своєму.
func (s *Screen) setScale(p int) bool {
	if !validScale(p) {
		return false
	}
	s.scale.Store(int32(p))
	s.mu.Lock()
	var move []*Client
	for _, v := range s.views {
		if v.scale == p {
			continue
		}
		for c := range v.clients {
			if c.scaleReq == 0 {
				move = append(move, c)
			}
		}
	}
	if len(move) > 0 {
		target := s.viewLocked(p)
		for _, c := range move {
			s.attachLocked(c, target, true)
		}
	}
	s.mu.Unlock()
	for _, c := range move {
		c.notify()
	}
	s.stale.Store(true) // змусити цикл захоплення перевипустити кадр
	s.poke()
	return true
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

// resizeView задає view новий розмір (змінилася область чи роздільність екрана) і
// повідомляє про це його клієнтів.
func (s *Screen) resizeView(v *view, w, h int, nb []byte) {
	s.mu.Lock()
	v.resize(w, h)
	copy(v.fb, nb)
	for c := range v.clients {
		c.mu.Lock()
		c.tw, c.th = v.tw, v.th
		c.dirty = make([]bool, v.tw*v.th)
		for i := range c.dirty {
			c.dirty[i] = true
		}
		c.spare = nil
		c.anyDirty, c.resized, c.fresh = true, true, true
		c.mu.Unlock()
		c.notify()
	}
	s.mu.Unlock()
}

// diff порівнює новий кадр із поточним поклітинно й заповнює v.changed. Читає v.fb
// без блокування: пише в нього лише цей самий потік.
func (v *view) diff(nb []byte) bool {
	any := false
	stride := v.w * 4
	for ty := 0; ty < v.th; ty++ {
		y0 := ty * tile
		y1 := y0 + tile
		if y1 > v.h {
			y1 = v.h
		}
		row := v.changed[ty*v.tw : (ty+1)*v.tw]
		for i := range row {
			row[i] = false
		}
		for y := y0; y < y1; y++ {
			off := y * stride
			if bytes.Equal(nb[off:off+stride], v.fb[off:off+stride]) {
				continue
			}
			for tx := 0; tx < v.tw; tx++ {
				if row[tx] {
					continue
				}
				a := off + tx*tile*4
				b := a + tile*4
				if b > off+stride {
					b = off + stride
				}
				if !bytes.Equal(nb[a:b], v.fb[a:b]) {
					row[tx] = true
					any = true
				}
			}
		}
	}
	return any
}

// publish переносить змінені клітинки у v.fb і позначає їх клієнтам цього view.
// Також позначає клієнтів «свіжими»: новий клієнт не отримує кадр, доки після його
// підключення не відбулося хоча б одне захоплення.
func (s *Screen) publish(v *view, nb []byte, any bool) {
	stride := v.w * 4
	s.mu.Lock()
	for ty := 0; any && ty < v.th; ty++ {
		y0 := ty * tile
		y1 := y0 + tile
		if y1 > v.h {
			y1 = v.h
		}
		for tx := 0; tx < v.tw; {
			if !v.changed[ty*v.tw+tx] {
				tx++
				continue
			}
			x0 := tx
			for tx < v.tw && v.changed[ty*v.tw+tx] {
				tx++
			}
			a := x0 * tile * 4
			b := tx * tile * 4
			if b > stride {
				b = stride
			}
			for y := y0; y < y1; y++ {
				copy(v.fb[y*stride+a:y*stride+b], nb[y*stride+a:y*stride+b])
			}
		}
	}
	for c := range v.clients {
		c.mu.Lock()
		if any && len(c.dirty) == len(v.changed) {
			for i, ch := range v.changed {
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
