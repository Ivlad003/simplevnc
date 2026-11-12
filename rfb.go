//go:build windows

package main

import (
	"bufio"
	"bytes"
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"sync"
	"time"
)

type Server struct {
	scr      *Screen
	password string // порожній — без пароля
	name     string

	mu    sync.Mutex
	fails map[string]*failRec
}

type failRec struct {
	n     int
	until time.Time
}

type Client struct {
	conn net.Conn
	scr  *Screen
	wmu  sync.Mutex // серіалізує записи в з'єднання

	encMu     sync.Mutex // захищає enc і прапорці кодувань
	enc       *encoder
	encPref   int32 // кодування, яке клієнт назвав першим серед підтримуваних
	canSize   bool
	addr      string
	extClip   bool   // клієнт уміє розширений буфер обміну в UTF-8 (захищено mu)
	apple     bool   // вбудований клієнт macOS (захищено mu)
	canCursor bool   // клієнт сам малює курсор (захищено mu)
	curSerial uint32 // яку форму курсора клієнт уже має (захищено mu)
	cbuf      []byte
	knownW    int // розмір кадру, про який клієнт уже знає
	knownH    int
	nReq      int // лічильники для журналу перших кадрів
	nUpd      int

	mu       sync.Mutex // захищає стан оновлень
	tw, th   int
	dirty    []bool
	spare    []bool
	anyDirty bool
	want     bool
	resized  bool
	fresh    bool // після підключення вже було захоплення екрана

	sig chan struct{}
	out bytes.Buffer
	rs  []rect
	in  inputState
}

func (c *Client) notify() {
	select {
	case c.sig <- struct{}{}:
	default:
	}
}

func vncResponse(pass string, challenge []byte) []byte {
	key := make([]byte, 8)
	copy(key, pass)
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	blk, _ := des.NewCipher(key)
	out := make([]byte, 16)
	blk.Encrypt(out[:8], challenge[:8])
	blk.Encrypt(out[8:], challenge[8:])
	return out
}

func (s *Server) blocked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[ip]
	return f != nil && time.Now().Before(f.until)
}

func (s *Server) authResult(ip string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		delete(s.fails, ip)
		return
	}
	f := s.fails[ip]
	if f == nil {
		f = &failRec{}
		s.fails[ip] = f
	}
	f.n++
	if f.n >= 5 { // після 5 невдач — пауза, що подвоюється
		d := 30 * time.Second << uint(min(f.n-5, 6))
		f.until = time.Now().Add(d)
	}
}

func writeReason(w io.Writer, msg string) {
	b := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(b, uint32(len(msg)))
	copy(b[4:], msg)
	w.Write(b)
}

// handshake виконує узгодження версії, автентифікацію та ініціалізацію.
func (s *Server) handshake(conn net.Conn) (w, h int, apple bool, err error) {
	w, h = s.scr.frameSize()
	err = s.handshakeSize(conn, w, h, &apple)
	return w, h, apple, err
}

// handshakeSize веде рукостискання й повідомляє клієнту розмір кадру w x h;
// у isApple записує, чи це вбудований клієнт macOS.
func (s *Server) handshakeSize(conn net.Conn, w, h int, isApple *bool) error {
	ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	// Час щедрий: клієнти (зокрема Screen Sharing на macOS) спершу з'єднуються,
	// а вже потім питають пароль у людини — сервер має дочекатися відповіді.
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	var ver [12]byte
	if _, err := io.ReadFull(conn, ver[:]); err != nil {
		return err
	}
	if string(ver[:4]) != "RFB " {
		return errors.New("не RFB-клієнт")
	}
	addr := conn.RemoteAddr().String()
	minor := int(ver[8]-'0')*100 + int(ver[9]-'0')*10 + int(ver[10]-'0')
	// Вбудований клієнт macOS (Screen Sharing) називає себе версією 003.889.
	// Як і libvncserver, вважаємо його клієнтом 3.8 з одним винятком нижче.
	apple := minor == 889
	*isApple = apple
	logf("%s: версія клієнта %q", addr, string(ver[:11]))
	if minor > 8 {
		minor = 8
	}

	if s.blocked(ip) {
		if minor >= 7 {
			conn.Write([]byte{0})
		} else {
			conn.Write([]byte{0, 0, 0, 0})
		}
		writeReason(conn, "Too many failed attempts, try later")
		return errors.New("забагато невдалих спроб")
	}

	sec := byte(1)
	if s.password != "" {
		sec = 2
	}
	if minor >= 7 {
		conn.Write([]byte{1, sec})
		var pick [1]byte
		if _, err := io.ReadFull(conn, pick[:]); err != nil {
			return err
		}
		if pick[0] != sec {
			return fmt.Errorf("клієнт обрав непідтримуваний тип захисту %d", pick[0])
		}
		logf("%s: обрано тип захисту %d, чекаю на пароль", addr, pick[0])
	} else {
		conn.Write([]byte{0, 0, 0, sec})
	}

	if sec == 2 {
		ch := make([]byte, 16)
		rand.Read(ch)
		conn.Write(ch)
		resp := make([]byte, 16)
		if _, err := io.ReadFull(conn, resp); err != nil {
			return err
		}
		ok := subtle.ConstantTimeCompare(resp, vncResponse(s.password, ch)) == 1
		s.authResult(ip, ok)
		if !ok {
			time.Sleep(time.Second)
			conn.Write([]byte{0, 0, 0, 1})
			if minor >= 8 {
				writeReason(conn, "Authentication failed")
			}
			return errors.New("неправильний пароль")
		}
		conn.Write([]byte{0, 0, 0, 0})
		logf("%s: пароль правильний", addr)
	} else if minor >= 8 && !apple {
		conn.Write([]byte{0, 0, 0, 0})
	}

	// Виняток із libvncserver: клієнт macOS, обравши захист «без пароля», не чекає
	// SecurityResult і не надсилає ClientInit — інакше обидві сторони чекали б вічно.
	if !(apple && sec == 1) {
		var shared [1]byte
		if _, err := io.ReadFull(conn, shared[:]); err != nil {
			return fmt.Errorf("клієнт розірвав з'єднання перед ClientInit: %w", err)
		}
	}

	init := make([]byte, 0, 24+len(s.name))
	init = binary.BigEndian.AppendUint16(init, uint16(w))
	init = binary.BigEndian.AppendUint16(init, uint16(h))
	init = append(init, nativePF.marshal()...)
	init = binary.BigEndian.AppendUint32(init, uint32(len(s.name)))
	init = append(init, s.name...)
	_, err := conn.Write(init)
	return err
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	addr := conn.RemoteAddr().String()
	logf("%s: нове з'єднання, чекаю на вхід", addr)
	w, h, apple, err := s.handshake(conn)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			logf("%s: відхилено (клієнт не завершив вхід за 3 хвилини)", addr)
		} else {
			logf("%s: відхилено (%v)", addr, err)
		}
		return
	}
	logf("%s: підключено", addr)

	c := &Client{conn: conn, scr: s.scr, enc: newEncoder(), sig: make(chan struct{}, 1), addr: addr, knownW: w, knownH: h, apple: apple}
	s.scr.add(c)
	done := make(chan struct{})
	go c.sender(done)

	err = c.reader()
	close(done)
	s.scr.remove(c)
	inputDo(c.in.releaseAll)
	logf("%s: відключено (%v)", addr, err)
}

// reader обробляє повідомлення клієнта.
func (c *Client) reader() error {
	br := bufio.NewReaderSize(c.conn, 4096)
	var b [20]byte
	for {
		typ, err := br.ReadByte()
		if err != nil {
			return err
		}
		switch typ {
		case 0: // SetPixelFormat
			if _, err := io.ReadFull(br, b[:19]); err != nil {
				return err
			}
			pf := parsePF(b[3:19])
			logf("%s: формат пікселів: %d біт, глибина %d, bigEndian=%v, trueColor=%v, макс %d/%d/%d, зсуви %d/%d/%d",
				c.addr, pf.bpp, pf.depth, pf.bigEndian, pf.trueColor, pf.rmax, pf.gmax, pf.bmax, pf.rs, pf.gs, pf.bs)
			c.encMu.Lock()
			c.enc.setFormat(pf)
			cmap := c.enc.cmap
			c.encMu.Unlock()
			c.mu.Lock()
			c.curSerial = 0 // форму курсора треба надіслати знову в новому форматі
			c.mu.Unlock()
			if cmap {
				c.sendColorMap()
			}
			c.markAll()
		case 2: // SetEncodings
			if _, err := io.ReadFull(br, b[:3]); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(b[1:3]))
			// Як у libvncserver: беремо перше з наших кодувань у порядку клієнта.
			pref, size, cursor, ext := int32(-1), false, false, false
			list := make([]int32, 0, n)
			for i := 0; i < n; i++ {
				if _, err := io.ReadFull(br, b[:4]); err != nil {
					return err
				}
				e := int32(binary.BigEndian.Uint32(b[:4]))
				list = append(list, e)
				switch e {
				case encZRLE, encZlib: // Raw лишається запасним: його вміє кожен клієнт
					if pref < 0 {
						pref = e
					}
				case encDesktopSize:
					size = true
				case encCursor:
					cursor = true
				case encExtClip:
					ext = true
				}
			}
			if pref < 0 {
				pref = encRaw
			}
			logf("%s: кодування клієнта %v, використовую %d", c.addr, list, pref)
			c.encMu.Lock()
			c.encPref, c.canSize = pref, size
			c.encMu.Unlock()
			c.mu.Lock()
			c.canCursor, c.curSerial = cursor, 0
			newExt := ext && !c.extClip
			c.extClip = ext
			c.mu.Unlock()
			if newExt {
				c.sendClipCaps()
			}
			c.scr.refreshCursorMode()
		case 3: // FramebufferUpdateRequest
			if _, err := io.ReadFull(br, b[:9]); err != nil {
				return err
			}
			if c.nReq < 3 { // перші запити — у журнал, для діагностики
				c.nReq++
				logf("%s: запит кадру: incremental=%d, область %d,%d %dx%d", c.addr, b[0],
					binary.BigEndian.Uint16(b[1:3]), binary.BigEndian.Uint16(b[3:5]),
					binary.BigEndian.Uint16(b[5:7]), binary.BigEndian.Uint16(b[7:9]))
			}
			c.mu.Lock()
			if b[0] == 0 { // повний кадр
				for i := range c.dirty {
					c.dirty[i] = true
				}
				c.anyDirty = true
			}
			c.want = true
			c.mu.Unlock()
			c.notify()
			c.scr.poke()
		case 4: // KeyEvent
			if _, err := io.ReadFull(br, b[:7]); err != nil {
				return err
			}
			sym, down := binary.BigEndian.Uint32(b[3:7]), b[0] != 0
			inputDo(func() { c.in.key(sym, down) })
			c.touch()
		case 5: // PointerEvent
			if _, err := io.ReadFull(br, b[:5]); err != nil {
				return err
			}
			px, py, mask := int(binary.BigEndian.Uint16(b[1:3])), int(binary.BigEndian.Uint16(b[3:5])), b[0]
			inputDo(func() { c.in.pointer(px, py, mask, int(c.scr.fbW.Load()), int(c.scr.fbH.Load())) })
			c.touch()
		case 6: // ClientCutText
			if _, err := io.ReadFull(br, b[:7]); err != nil {
				return err
			}
			n := int32(binary.BigEndian.Uint32(b[3:7]))
			extended := n < 0 // від'ємна довжина — розширений буфер обміну
			if extended {
				n = -n
			}
			if n < 0 || n > 8<<20 {
				return errors.New("завеликий буфер обміну")
			}
			txt := make([]byte, n)
			if _, err := io.ReadFull(br, txt); err != nil {
				return err
			}
			if extended {
				c.handleExtClip(txt)
			} else {
				setClipboard(txt)
			}
		default:
			return fmt.Errorf("невідоме повідомлення клієнта: тип %d", typ)
		}
	}
}

func (c *Client) touch() {
	c.scr.lastInput.Store(time.Now().UnixNano())
	c.scr.poke()
}

func (c *Client) markAll() {
	c.mu.Lock()
	for i := range c.dirty {
		c.dirty[i] = true
	}
	c.anyDirty = true
	c.mu.Unlock()
	c.notify()
}

func (c *Client) write(p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.conn.Write(p)
	return err
}

// sendColorMap шле палітру BGR233 клієнтам без true-color.
func (c *Client) sendColorMap() {
	m := make([]byte, 6+256*6)
	m[0] = 1
	binary.BigEndian.PutUint16(m[4:], 256)
	for i := 0; i < 256; i++ {
		o := 6 + i*6
		binary.BigEndian.PutUint16(m[o:], uint16((i&7)*65535/7))
		binary.BigEndian.PutUint16(m[o+2:], uint16((i>>3&7)*65535/7))
		binary.BigEndian.PutUint16(m[o+4:], uint16((i>>6&3)*65535/3))
	}
	c.write(m)
}

func (c *Client) sender(done chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-c.sig:
		}
		if err := c.sendUpdate(); err != nil {
			c.conn.Close()
			return
		}
	}
}

// sendUpdate шле одне FramebufferUpdate, якщо клієнт його чекає і є зміни.
func (c *Client) sendUpdate() error {
	s := c.scr
	s.mu.RLock()
	c.mu.Lock()
	// курсор на боці клієнта: справжня форма або прозорий, якщо курсор уже в кадрі
	var curWant uint32
	var curShape *cursorShape
	if c.canCursor {
		if cursorInFrame.Load() {
			curWant = 1
		} else if s.cur != nil {
			curWant, curShape = s.curSerial, s.cur
		}
	}
	curPending := curWant != 0 && curWant != c.curSerial
	if !c.want || !c.fresh || (!c.anyDirty && !curPending) || len(c.dirty) != s.tw*s.th {
		c.mu.Unlock()
		s.mu.RUnlock()
		return nil
	}
	if c.resized && c.knownW == s.w && c.knownH == s.h {
		c.resized = false // клієнт уже має цей розмір із ServerInit
	}
	c.want = false
	if c.resized {
		c.resized = false
		c.knownW, c.knownH = s.w, s.h
		c.mu.Unlock()
		w, h := s.w, s.h
		s.mu.RUnlock()
		c.encMu.Lock()
		ok := c.canSize
		c.encMu.Unlock()
		if !ok {
			return errors.New("розмір екрана змінився")
		}
		m := []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(m[8:], uint16(w))
		binary.BigEndian.PutUint16(m[10:], uint16(h))
		binary.BigEndian.PutUint32(m[12:], uint32(0xFFFFFFFF-222)) // -223
		return c.write(m)
	}
	var dirty []bool
	if c.anyDirty {
		dirty = c.dirty
		if len(c.spare) == len(dirty) {
			c.dirty = c.spare
		} else {
			c.dirty = make([]bool, len(dirty))
		}
		c.spare = nil
		c.anyDirty = false
	}
	if curPending {
		c.curSerial = curWant
	}
	c.mu.Unlock()

	c.rs = c.rs[:0]
	if dirty != nil {
		c.rs = mergeRects(dirty, s.tw, s.th, s.w, s.h, c.rs)
	}
	nRects := len(c.rs)
	if curPending {
		nRects++
	}
	stride := s.w * 4
	c.out.Reset()
	c.out.Write([]byte{0, 0, byte(nRects >> 8), byte(nRects)})
	c.encMu.Lock()
	if curPending {
		c.cbuf = c.enc.cursorRect(c.cbuf[:0], curShape)
		c.out.Write(c.cbuf)
	}
	var hdr [12]byte
	for _, r := range c.rs {
		binary.BigEndian.PutUint16(hdr[0:], uint16(r.x))
		binary.BigEndian.PutUint16(hdr[2:], uint16(r.y))
		binary.BigEndian.PutUint16(hdr[4:], uint16(r.w))
		binary.BigEndian.PutUint16(hdr[6:], uint16(r.h))
		binary.BigEndian.PutUint32(hdr[8:], uint32(c.encPref))
		c.out.Write(hdr[:])
		switch c.encPref {
		case encZRLE:
			c.enc.zrleRect(&c.out, s.fb, stride, r)
		case encZlib:
			c.enc.zlibRect(&c.out, s.fb, stride, r)
		default:
			c.enc.rawRect(&c.out, s.fb, stride, r)
		}
	}
	pref := c.encPref
	c.encMu.Unlock()
	s.mu.RUnlock()

	err := c.write(c.out.Bytes())
	if c.nUpd < 3 { // перші кадри — у журнал, для діагностики
		c.nUpd++
		logf("%s: надіслано кадр: %d прямокутників, %d байт, кодування %d, помилка: %v",
			c.addr, len(c.rs), c.out.Len(), pref, err)
	}

	if dirty != nil {
		for i := range dirty {
			dirty[i] = false
		}
		c.mu.Lock()
		c.spare = dirty
		c.mu.Unlock()
	}
	return err
}
