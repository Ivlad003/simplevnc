//go:build windows

package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var ownClipSeq atomic.Uint32 // номер зміни буфера, яку зробили ми самі

func openClipboard() bool {
	for i := 0; i < 10; i++ {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// setClipboard кладе текст від клієнта в буфер обміну Windows.
func setClipboard(b []byte) {
	var s string
	if utf8.Valid(b) {
		s = string(b)
	} else { // Latin-1 за стандартом RFB
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		s = string(r)
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	u := append(utf16.Encode([]rune(s)), 0)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !openClipboard() {
		return
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if h == 0 {
		return
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return
	}
	pRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)*2))
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h)
		return
	}
	seq, _, _ := pGetClipboardSequenceNumber.Call()
	ownClipSeq.Store(uint32(seq))
}

func getClipboard() (string, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !openClipboard() {
		return "", false
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return "", false
	}
	defer pGlobalUnlock.Call(h)
	var u []uint16
	for i := 0; i < 1<<20; i++ {
		c := *(*uint16)(unsafe.Pointer(p + uintptr(i)*2))
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u)), true
}

// Розширений буфер обміну (псевдокодування 0xC0A1E5CE): текст у UTF-8, тож
// кирилиця не губиться. Його вміють TigerVNC і noVNC. Сторона, де буфер змінився,
// шле «notify», інша відповідає «request» і отримує «provide» зі стиснутим текстом.
const (
	encExtClip = -1063131698

	clipText    = 1
	clipCaps    = 1 << 24
	clipRequest = 1 << 25
	clipPeek    = 1 << 26
	clipNotify  = 1 << 27
	clipProvide = 1 << 28
	clipMaxText = 4 << 20
)

// extClipMsg — ServerCutText із від'ємною довжиною: прапорці й дані.
func extClipMsg(flags uint32, payload []byte) []byte {
	m := make([]byte, 8, 12+len(payload))
	m[0] = 3
	binary.BigEndian.PutUint32(m[4:], uint32(-int32(4+len(payload))))
	m = binary.BigEndian.AppendUint32(m, flags)
	return append(m, payload...)
}

func (c *Client) sendClipCaps() {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], clipMaxText)
	c.write(extClipMsg(clipCaps|clipText|clipRequest|clipPeek|clipNotify|clipProvide, size[:]))
}

// sendClipProvide віддає клієнту поточний текст буфера обміну.
func (c *Client) sendClipProvide() {
	txt, _ := getClipboard()
	txt = strings.ReplaceAll(strings.ReplaceAll(txt, "\r\n", "\n"), "\n", "\r\n")
	if len(txt) > clipMaxText {
		txt = txt[:clipMaxText]
	}
	raw := binary.BigEndian.AppendUint32(nil, uint32(len(txt)+1))
	raw = append(append(raw, txt...), 0)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(raw)
	zw.Close()
	c.write(extClipMsg(clipProvide|clipText, z.Bytes()))
}

// handleExtClip обробляє розширене повідомлення буфера обміну від клієнта.
func (c *Client) handleExtClip(data []byte) {
	if len(data) < 4 {
		return
	}
	flags, body := binary.BigEndian.Uint32(data), data[4:]
	switch {
	case flags&clipCaps != 0: // можливості клієнта: досить знати, що вони є
	case flags&clipNotify != 0: // у клієнта новий текст — просимо його
		if flags&clipText != 0 {
			c.write(extClipMsg(clipRequest|clipText, nil))
		}
	case flags&clipPeek != 0:
		c.write(extClipMsg(clipNotify|clipText, nil))
	case flags&clipRequest != 0:
		if flags&clipText != 0 {
			c.sendClipProvide()
		}
	case flags&clipProvide != 0:
		if flags&clipText == 0 {
			return
		}
		// Читаємо рівно стільки, скільки заявлено: частина клієнтів не закриває
		// zlib-потік, і читання «до кінця» дало б помилку.
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return
		}
		var n [4]byte
		if _, err := io.ReadFull(zr, n[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(n[:])
		if size > clipMaxText {
			return
		}
		txt := make([]byte, size)
		if _, err := io.ReadFull(zr, txt); err != nil {
			return
		}
		setClipboard(bytes.TrimRight(txt, "\x00"))
	}
}

// watchClipboard повідомляє клієнтів про текст, скопійований на сервері.
func (s *Screen) watchClipboard() {
	last, _, _ := pGetClipboardSequenceNumber.Call()
	for {
		time.Sleep(400 * time.Millisecond)
		seq, _, _ := pGetClipboardSequenceNumber.Call()
		if seq == last {
			continue
		}
		last = seq
		if uint32(seq) == ownClipSeq.Load() {
			continue
		}
		cl := s.allClients()
		if len(cl) == 0 {
			continue
		}
		txt, ok := getClipboard()
		if !ok {
			continue
		}
		txt = strings.ReplaceAll(txt, "\r\n", "\n")
		// Класичний ServerCutText за стандартом несе Latin-1. Клієнту macOS шлемо
		// UTF-8: інакше кирилиця точно перетворилася б на знаки питання.
		latin := make([]byte, 8, 8+len(txt))
		latin[0] = 3
		for _, r := range txt {
			if r > 0xff {
				r = '?'
			}
			latin = append(latin, byte(r))
		}
		binary.BigEndian.PutUint32(latin[4:], uint32(len(latin)-8))
		utf := append([]byte{3, 0, 0, 0, 0, 0, 0, 0}, txt...)
		binary.BigEndian.PutUint32(utf[4:], uint32(len(txt)))
		for _, c := range cl {
			c.mu.Lock()
			ext, apple := c.extClip, c.apple
			c.mu.Unlock()
			switch {
			case ext: // текст піде лише тоді, коли клієнт його попросить
				go c.write(extClipMsg(clipNotify|clipText, nil))
			case apple:
				logf("%s: буфер обміну VNC: клієнту macOS %d байт (UTF-8)", c.addr, len(txt))
				go c.write(utf)
			default:
				logf("%s: буфер обміну VNC: клієнту %d байт (Latin-1)", c.addr, len(latin)-8)
				go c.write(latin)
			}
		}
	}
}
