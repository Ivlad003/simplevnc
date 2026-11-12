//go:build windows

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"unsafe"
)

const (
	encRaw         = 0
	encZlib        = 6
	encZRLE        = 16
	encDesktopSize = -223
)

type pixFmt struct {
	bpp, depth       uint8
	bigEndian        bool
	trueColor        bool
	rmax, gmax, bmax uint16
	rs, gs, bs       uint8
}

// Рідний формат сервера: 32 біти, BGRX у пам'яті.
var nativePF = pixFmt{32, 24, false, true, 255, 255, 255, 16, 8, 0}

func (p pixFmt) marshal() []byte {
	b := make([]byte, 16)
	b[0], b[1] = p.bpp, p.depth
	if p.bigEndian {
		b[2] = 1
	}
	if p.trueColor {
		b[3] = 1
	}
	binary.BigEndian.PutUint16(b[4:], p.rmax)
	binary.BigEndian.PutUint16(b[6:], p.gmax)
	binary.BigEndian.PutUint16(b[8:], p.bmax)
	b[10], b[11], b[12] = p.rs, p.gs, p.bs
	return b
}

func parsePF(b []byte) pixFmt {
	return pixFmt{
		bpp: b[0], depth: b[1], bigEndian: b[2] != 0, trueColor: b[3] != 0,
		rmax: binary.BigEndian.Uint16(b[4:]), gmax: binary.BigEndian.Uint16(b[6:]),
		bmax: binary.BigEndian.Uint16(b[8:]),
		rs:   b[10], gs: b[11], bs: b[12],
	}
}

type rect struct{ x, y, w, h int }

// mergeRects збирає позначені клітинки в прямокутники (горизонтальні серії,
// однакові серії в сусідніх рядах зливаються вертикально).
func mergeRects(dirty []bool, tw, th, w, h int, out []rect) []rect {
	out = out[:0]
	type run struct{ x, w, idx int }
	var prev, cur []run // серії попереднього й поточного ряду
	for ty := 0; ty < th; ty++ {
		cur = cur[:0]
		row := dirty[ty*tw : (ty+1)*tw]
		for tx := 0; tx < tw; {
			if !row[tx] {
				tx++
				continue
			}
			x0 := tx
			for tx < tw && row[tx] {
				tx++
			}
			r := rect{x0 * tile, ty * tile, (tx - x0) * tile, tile}
			if r.x+r.w > w {
				r.w = w - r.x
			}
			if r.y+r.h > h {
				r.h = h - r.y
			}
			merged := false
			for _, p := range prev {
				if p.x == r.x && p.w == r.w {
					out[p.idx].h += r.h
					cur = append(cur, p)
					merged = true
					break
				}
			}
			if !merged {
				out = append(out, r)
				cur = append(cur, run{r.x, r.w, len(out) - 1})
			}
		}
		prev, cur = cur, prev
	}
	return out
}

// encoder — стан кодування одного клієнта.
type encoder struct {
	pf     pixFmt
	native bool
	cmap   bool // клієнт просив палітру — шлемо BGR233 + карту кольорів
	lut    [3][256]uint32
	bypp   int    // байтів на піксель
	cpix   int    // байтів на CPIXEL (ZRLE)
	cshift uint   // зсув перед записом 3-байтного CPIXEL
	fill   uint32 // біти поза кольором (альфа), які шлемо одиницями
	row    []byte

	raw   []byte
	zbuf  bytes.Buffer
	zw    *zlib.Writer // потік ZRLE
	zbuf2 bytes.Buffer
	zw2   *zlib.Writer // окремий потік кодування Zlib
	px    [64 * 64]uint32
	idx   [64 * 64]uint8
}

func newEncoder() *encoder {
	e := &encoder{}
	e.zw, _ = zlib.NewWriterLevel(&e.zbuf, flate.BestSpeed)
	e.setFormat(nativePF)
	return e
}

func (e *encoder) setFormat(p pixFmt) {
	e.cmap = false
	if !p.trueColor || (p.bpp != 8 && p.bpp != 16 && p.bpp != 32) {
		e.cmap = !p.trueColor
		p = pixFmt{8, 8, false, true, 7, 7, 3, 0, 3, 6}
	}
	e.pf = p
	// Як у готових серверах, поле depth не впливає на розкладку пікселя.
	e.native = p.bpp == 32 && !p.bigEndian && p.rmax == 255 && p.gmax == 255 && p.bmax == 255 &&
		p.rs == 16 && p.gs == 8 && p.bs == 0
	e.bypp = int(p.bpp) / 8
	for v := 0; v < 256; v++ {
		e.lut[0][v] = uint32((v*int(p.rmax)+127)/255) << (p.rs & 31)
		e.lut[1][v] = uint32((v*int(p.gmax)+127)/255) << (p.gs & 31)
		e.lut[2][v] = uint32((v*int(p.bmax)+127)/255) << (p.bs & 31)
	}
	// Правило CPIXEL як у libvncserver і TigerVNC: depth не враховується, важить
	// лише те, чи вміщаються біти кольору в молодші або старші 3 байти.
	e.cpix, e.cshift, e.fill = e.bypp, 0, 0
	if p.bpp == 32 {
		max := e.lut[0][255] | e.lut[1][255] | e.lut[2][255]
		// Невикористані біти (альфа) заповнюємо одиницями: частина клієнтів,
		// зокрема на macOS, трактує їх як прозорість і малює чорне.
		e.fill = ^max
		ls3, ms3 := max < 1<<24, max&0xff == 0
		switch {
		case ls3 && (!p.bigEndian || !ms3):
			e.cpix = 3
		case ms3:
			e.cpix, e.cshift = 3, 8
		}
	}
}

func (e *encoder) conv(p uint32) uint32 {
	return e.lut[0][(p>>16)&0xff] | e.lut[1][(p>>8)&0xff] | e.lut[2][p&0xff] | e.fill
}

// appendC дописує CPIXEL (p — рідний 0x00RRGGBB).
func (e *encoder) appendC(dst []byte, p uint32) []byte {
	if e.native {
		return append(dst, byte(p), byte(p>>8), byte(p>>16))
	}
	v := e.conv(p)
	switch e.cpix {
	case 1:
		return append(dst, byte(v))
	case 2:
		if e.pf.bigEndian {
			return append(dst, byte(v>>8), byte(v))
		}
		return append(dst, byte(v), byte(v>>8))
	case 3:
		v >>= e.cshift
		if e.pf.bigEndian {
			return append(dst, byte(v>>16), byte(v>>8), byte(v))
		}
		return append(dst, byte(v), byte(v>>8), byte(v>>16))
	}
	if e.pf.bigEndian {
		return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// appendP дописує повний піксель у форматі клієнта.
func (e *encoder) appendP(dst []byte, p uint32) []byte {
	v := e.conv(p)
	switch e.bypp {
	case 1:
		return append(dst, byte(v))
	case 2:
		if e.pf.bigEndian {
			return append(dst, byte(v>>8), byte(v))
		}
		return append(dst, byte(v), byte(v>>8))
	}
	if e.pf.bigEndian {
		return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func rowPixels(fb []byte, off, n int) []uint32 {
	return unsafe.Slice((*uint32)(unsafe.Pointer(&fb[off])), n)
}

// opaqueRow повертає рядок рідних пікселів з альфою 255 (GDI лишає там нулі).
func (e *encoder) opaqueRow(fb []byte, off, n int) []byte {
	if cap(e.row) < n*4 {
		e.row = make([]byte, n*4)
	}
	dst := unsafe.Slice((*uint32)(unsafe.Pointer(&e.row[0])), n)
	for i, p := range rowPixels(fb, off, n) {
		dst[i] = p | 0xff000000
	}
	return e.row[:n*4]
}

// rawRect — кодування Raw.
func (e *encoder) rawRect(out *bytes.Buffer, fb []byte, stride int, r rect) {
	if e.native {
		for y := r.y; y < r.y+r.h; y++ {
			out.Write(e.opaqueRow(fb, y*stride+r.x*4, r.w))
		}
		return
	}
	for y := r.y; y < r.y+r.h; y++ {
		e.raw = e.raw[:0]
		for _, p := range rowPixels(fb, y*stride+r.x*4, r.w) {
			e.raw = e.appendP(e.raw, p)
		}
		out.Write(e.raw)
	}
}

// zlibRect — кодування Zlib: ті самі сирі пікселі, стиснуті одним потоком.
// Запасний варіант для клієнтів без ZRLE.
func (e *encoder) zlibRect(out *bytes.Buffer, fb []byte, stride int, r rect) {
	if e.zw2 == nil {
		e.zw2, _ = zlib.NewWriterLevel(&e.zbuf2, flate.BestSpeed)
	}
	e.zbuf2.Reset()
	for y := r.y; y < r.y+r.h; y++ {
		off := y*stride + r.x*4
		if e.native {
			e.zw2.Write(e.opaqueRow(fb, off, r.w))
			continue
		}
		e.raw = e.raw[:0]
		for _, p := range rowPixels(fb, off, r.w) {
			e.raw = e.appendP(e.raw, p)
		}
		e.zw2.Write(e.raw)
	}
	e.zw2.Flush()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(e.zbuf2.Len()))
	out.Write(n[:])
	out.Write(e.zbuf2.Bytes())
}

// zrleRect — кодування ZRLE: плитки 64x64, один zlib-потік на з'єднання.
func (e *encoder) zrleRect(out *bytes.Buffer, fb []byte, stride int, r rect) {
	e.raw = e.raw[:0]
	for ty := r.y; ty < r.y+r.h; ty += 64 {
		th := r.y + r.h - ty
		if th > 64 {
			th = 64
		}
		for tx := r.x; tx < r.x+r.w; tx += 64 {
			tw := r.x + r.w - tx
			if tw > 64 {
				tw = 64
			}
			e.zrleTile(fb, stride, tx, ty, tw, th)
		}
	}
	e.zbuf.Reset()
	e.zw.Write(e.raw)
	e.zw.Flush()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(e.zbuf.Len()))
	out.Write(n[:])
	out.Write(e.zbuf.Bytes())
}

func (e *encoder) zrleTile(fb []byte, stride, x, y, w, h int) {
	n := w * h
	px := e.px[:n]
	i := 0
	for yy := 0; yy < h; yy++ {
		for _, p := range rowPixels(fb, (y+yy)*stride+x*4, w) {
			px[i] = p & 0xffffff
			i++
		}
	}

	// палітра до 16 кольорів
	var pal [16]uint32
	np := 0
	idx := e.idx[:n]
	last, lastIdx := ^uint32(0), uint8(0)
	small := true
	for i, p := range px {
		if p != last {
			j := 0
			for j < np && pal[j] != p {
				j++
			}
			if j == np {
				if np == 16 {
					small = false
					break
				}
				pal[np] = p
				np++
			}
			last, lastIdx = p, uint8(j)
		}
		idx[i] = lastIdx
	}

	out := e.raw
	switch {
	case small && np == 1: // суцільний колір
		out = append(out, 1)
		out = e.appendC(out, pal[0])
	case small: // упакована палітра
		out = append(out, byte(np))
		for j := 0; j < np; j++ {
			out = e.appendC(out, pal[j])
		}
		bits := uint(4)
		if np == 2 {
			bits = 1
		} else if np <= 4 {
			bits = 2
		}
		for yy := 0; yy < h; yy++ {
			var acc byte
			var nb uint
			for _, v := range idx[yy*w : yy*w+w] {
				acc = acc<<bits | v
				nb += bits
				if nb == 8 {
					out = append(out, acc)
					acc, nb = 0, 0
				}
			}
			if nb > 0 {
				out = append(out, acc<<(8-nb))
			}
		}
	default: // сирі CPIXEL
		out = append(out, 0)
		if e.native {
			for _, p := range px {
				out = append(out, byte(p), byte(p>>8), byte(p>>16))
			}
		} else {
			for _, p := range px {
				out = e.appendC(out, p)
			}
		}
	}
	e.raw = out
}
