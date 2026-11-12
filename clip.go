//go:build windows

package main

import (
	"encoding/binary"
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

// watchClipboard шле клієнтам текст, скопійований на сервері (ServerCutText, Latin-1).
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
		s.mu.RLock()
		cl := make([]*Client, 0, len(s.clients))
		for c := range s.clients {
			cl = append(cl, c)
		}
		s.mu.RUnlock()
		if len(cl) == 0 {
			continue
		}
		txt, ok := getClipboard()
		if !ok {
			continue
		}
		txt = strings.ReplaceAll(txt, "\r\n", "\n")
		m := make([]byte, 8, 8+len(txt))
		m[0] = 3
		for _, r := range txt {
			if r > 0xff {
				r = '?'
			}
			m = append(m, byte(r))
		}
		binary.BigEndian.PutUint32(m[4:], uint32(len(m)-8))
		for _, c := range cl {
			go c.write(m)
		}
	}
}
