//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

// Захоплення через DXGI Desktop Duplication: кадр береться з GPU і лише тоді,
// коли екран справді змінився. Курсор домальовується через GDI в ту саму DIB.

var (
	d3d11              = syscall.NewLazyDLL("d3d11.dll")
	dxgi               = syscall.NewLazyDLL("dxgi.dll")
	pD3D11CreateDevice = d3d11.NewProc("D3D11CreateDevice")
	pCreateDXGIFactory = dxgi.NewProc("CreateDXGIFactory1")
)

type guid struct {
	d1 uint32
	d2 uint16
	d3 uint16
	d4 [8]byte
}

var (
	iidFactory1  = guid{0x770aae78, 0xf26f, 0x4dba, [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidOutput1   = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidTexture2D = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

const (
	dxgiWaitTimeout = 0x887A0027
	dxgiFormatBGRA  = 87
)

type dxgiOutputDesc struct {
	name                     [32]uint16
	left, top, right, bottom int32
	attached                 int32
	rotation                 uint32
	monitor                  uintptr
}

type dxgiFrameInfo struct {
	lastPresent     int64
	lastMouse       int64
	accumulated     uint32
	rectsCoalesced  int32
	protectedMasked int32
	ptrX, ptrY      int32
	ptrVisible      int32
	metaSize        uint32
	shapeSize       uint32
}

type d3dTexDesc struct {
	w, h, mip, arr, format     uint32
	sampleCount, sampleQuality uint32
	usage, bind, cpu, misc     uint32
}

type d3dMapped struct {
	data                 uintptr
	rowPitch, depthPitch uint32
}

// comCall викликає метод COM-об'єкта за індексом у vtable.
func comCall(obj uintptr, idx int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	a := make([]uintptr, 0, 8)
	a = append(a, obj)
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(fn, a...)
	return r
}

func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

func release(obj uintptr) {
	if obj != 0 {
		comCall(obj, 2)
	}
}

// dxOut — один монітор.
type dxOut struct {
	dev, ctx, dup, staging uintptr
	x, y, w, h             int // положення у віртуальному екрані
	have                   bool
}

type dxgiCap struct {
	*gdiCap // DIB-буфер кадру та малювання курсора
	outs    []dxOut
	ready   bool
	drawn   bool // чи курсор намальовано в останньому зібраному кадрі
	created time.Time
	alive   time.Time // коли DXGI востаннє щось віддав: зображення або рух миші
}

// dxgiBadUntil — до цього часу DXGI не пробуємо (не віддав зображення).
var dxgiBadUntil time.Time

// dxgiNoImage — DXGI під'єднався, але зображення не віддав. Так буває, коли монітор
// вимкнено чи від'єднано (ПК без монітора): Windows малює робочий стіл, але на екран
// його не виводить, і DXGI нема чого віддавати. GDI при цьому працює.
var dxgiNoImage bool

func newDxgiCap() *dxgiCap {
	if pD3D11CreateDevice.Find() != nil || pCreateDXGIFactory.Find() != nil ||
		time.Now().Before(dxgiBadUntil) {
		return nil
	}
	g := newGdiCap()
	if g == nil {
		return nil
	}
	c := &dxgiCap{gdiCap: g, created: time.Now(), alive: time.Now()}
	if !c.init() || len(c.outs) == 0 {
		if verbose {
			logf("DXGI: не вдалося почати захоплення (виходів: %d)", len(c.outs))
		}
		c.close()
		return nil
	}
	return c
}

func (c *dxgiCap) init() bool {
	var factory uintptr
	if failed(pCreateDXGIFactoryCall(&factory)) {
		return false
	}
	defer release(factory)
	for ai := 0; ; ai++ {
		var adapter uintptr
		if failed(comCall(factory, 12, uintptr(ai), uintptr(unsafe.Pointer(&adapter)))) { // EnumAdapters1
			break
		}
		ok := c.addAdapter(adapter)
		release(adapter)
		if !ok {
			return false
		}
	}
	return true
}

func pCreateDXGIFactoryCall(out *uintptr) uintptr {
	r, _, _ := pCreateDXGIFactory.Call(uintptr(unsafe.Pointer(&iidFactory1)), uintptr(unsafe.Pointer(out)))
	return r
}

// addAdapter підключає всі монітори адаптера; false — DXGI використати не можна.
func (c *dxgiCap) addAdapter(adapter uintptr) bool {
	var dev, ctx uintptr
	for oi := 0; ; oi++ {
		var output uintptr
		if failed(comCall(adapter, 7, uintptr(oi), uintptr(unsafe.Pointer(&output)))) { // EnumOutputs
			break
		}
		var d dxgiOutputDesc
		comCall(output, 7, uintptr(unsafe.Pointer(&d))) // GetDesc
		if d.attached == 0 || int(d.right) <= c.vx || int(d.left) >= c.vx+c.w ||
			int(d.bottom) <= c.vy || int(d.top) >= c.vy+c.h {
			release(output) // вимкнений монітор або поза областю трансляції
			continue
		}
		if d.rotation > 1 { // повернутий монітор — простіше через GDI
			release(output)
			release(dev)
			release(ctx)
			return false
		}
		if dev == 0 {
			var level uint32
			r, _, _ := pD3D11CreateDevice.Call(adapter, 0, 0, 0, 0, 0, 7,
				uintptr(unsafe.Pointer(&dev)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&ctx)))
			if failed(r) || dev == 0 {
				release(output)
				return false
			}
		}
		var out1, dup uintptr
		hr := comCall(output, 0, uintptr(unsafe.Pointer(&iidOutput1)), uintptr(unsafe.Pointer(&out1)))
		release(output)
		if failed(hr) {
			release(dev)
			release(ctx)
			return false
		}
		hr = comCall(out1, 22, dev, uintptr(unsafe.Pointer(&dup))) // DuplicateOutput
		release(out1)
		if failed(hr) || dup == 0 {
			if verbose {
				logf("DXGI: DuplicateOutput повернув %#x", uint32(hr))
			}
			release(dev)
			release(ctx)
			return false
		}
		// кожен вихід тримає власне посилання на пристрій і контекст
		comCall(dev, 1)
		comCall(ctx, 1)
		c.outs = append(c.outs, dxOut{
			dev: dev, ctx: ctx, dup: dup,
			x: int(d.left) - c.vx, y: int(d.top) - c.vy,
			w: int(d.right - d.left), h: int(d.bottom - d.top),
		})
	}
	release(dev)
	release(ctx)
	return true
}

func (c *dxgiCap) close() {
	for i := range c.outs {
		o := &c.outs[i]
		release(o.staging)
		release(o.dup)
		release(o.ctx)
		release(o.dev)
	}
	c.outs = nil
	if c.gdiCap != nil {
		c.gdiCap.close()
	}
}

// grab чекає на новий кадр до wait. changed=false — екран не змінювався.
func (c *dxgiCap) grab(wait time.Duration) (changed, ok bool) {
	start := time.Now()
	image := false
	for i := range c.outs {
		o := &c.outs[i]
		timeout := uintptr(0)
		if len(c.outs) == 1 {
			timeout = uintptr(wait / time.Millisecond)
		}
		var info dxgiFrameInfo
		var res uintptr
		hr := comCall(o.dup, 8, timeout, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&res))) // AcquireNextFrame
		if uint32(hr) == dxgiWaitTimeout {
			continue
		}
		if failed(hr) {
			return false, false // доступ втрачено (зміна режиму, UAC) — перестворюємо
		}
		c.alive = time.Now()
		// Зображення є лише коли був реальний показ кадру; перша подія може
		// бути самим рухом миші — тоді текстура ще порожня.
		good := true
		if info.lastPresent != 0 || info.accumulated != 0 {
			good = o.copyFrame(res)
			image = true
		}
		release(res)
		comCall(o.dup, 14) // ReleaseFrame
		if !good {
			return false, false
		}
		changed = true
	}
	for i := range c.outs {
		if !c.outs[i].have { // ще не всі монітори віддали перше зображення
			if time.Since(c.created) > 1500*time.Millisecond {
				dxgiBadUntil = time.Now().Add(time.Minute)
				if !dxgiNoImage {
					dxgiNoImage = true
					logf("DXGI не віддає зображення (так буває, коли монітор вимкнено або від'єднано) — працюю через GDI")
				}
				return false, false // переходимо на GDI або лишаємося на ньому
			}
			time.Sleep(5 * time.Millisecond)
			return false, true
		}
	}
	if !changed && len(c.outs) > 1 {
		if d := wait - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
	// Збираємо кадр, коли змінилося зображення; сам рух миші важить лише тоді,
	// коли курсор домальовується в кадр.
	inFrame := cursorInFrame.Load()
	if c.ready && !image && !(changed && inFrame) && inFrame == c.drawn {
		return false, true
	}
	for i := range c.outs {
		if !c.outs[i].compose(c.gdiCap) {
			return false, false
		}
	}
	if inFrame {
		c.drawCursor()
	}
	c.ready, c.drawn = true, inFrame
	dxgiNoImage = false
	return true, true
}

// copyFrame копіює знімок екрана з GPU у staging-текстуру, доступну процесору.
func (o *dxOut) copyFrame(res uintptr) bool {
	var tex uintptr
	if failed(comCall(res, 0, uintptr(unsafe.Pointer(&iidTexture2D)), uintptr(unsafe.Pointer(&tex)))) {
		return false
	}
	defer release(tex)
	if o.staging == 0 {
		var d d3dTexDesc
		comCall(tex, 10, uintptr(unsafe.Pointer(&d))) // GetDesc
		if d.format != dxgiFormatBGRA {
			return false
		}
		o.w, o.h = int(d.w), int(d.h)
		d.mip, d.arr, d.sampleCount, d.sampleQuality = 1, 1, 1, 0
		d.usage, d.bind, d.cpu, d.misc = 3, 0, 0x20000, 0                                                   // STAGING, CPU_ACCESS_READ
		if failed(comCall(o.dev, 5, uintptr(unsafe.Pointer(&d)), 0, uintptr(unsafe.Pointer(&o.staging)))) { // CreateTexture2D
			return false
		}
	}
	comCall(o.ctx, 47, o.staging, tex) // CopyResource
	o.have = true
	return true
}

// compose переносить знімок монітора в спільний буфер кадру.
func (o *dxOut) compose(g *gdiCap) bool {
	if !o.have {
		return true
	}
	var m d3dMapped
	if failed(comCall(o.ctx, 14, o.staging, 0, 1, 0, uintptr(unsafe.Pointer(&m)))) || m.data == 0 { // Map(READ)
		return false
	}
	pitch := int(m.rowPitch)
	src := unsafe.Slice((*byte)(unsafe.Pointer(m.data)), pitch*o.h)
	x0, x1 := max(o.x, 0), min(o.x+o.w, g.w)
	for y := max(o.y, 0); y < min(o.y+o.h, g.h) && x0 < x1; y++ {
		s := (y-o.y)*pitch + (x0-o.x)*4
		copy(g.bits[(y*g.w+x0)*4:(y*g.w+x1)*4], src[s:s+(x1-x0)*4])
	}
	comCall(o.ctx, 15, o.staging, 0) // Unmap
	return true
}
