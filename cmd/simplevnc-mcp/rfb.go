package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"sync"
	"time"
)

// VNC-клієнт (RFB 3.8): тримає з'єднання з сервером, постійно оновлює копію
// екрана в пам'яті й надсилає події миші та клавіатури.

const (
	encRaw         = 0
	encZRLE        = 16
	encDesktopSize = -223
	// Приватне псевдокодування SimpleVNC: «вводь саме ті символи, які я шлю».
	// Інші сервери його не знають і просто ігнорують.
	encExactKeys = 0x53564E01
)

type vncClient struct {
	addr, password string

	mu         sync.Mutex
	w, h       int
	fb         []byte // RGBA, w*h*4
	name       string
	ready      bool      // отримано хоча б один повний кадр
	lastUpdate time.Time // коли прийшло останнє оновлення
	lastErr    error
	conn       net.Conn

	wmu sync.Mutex // серіалізує записи в з'єднання
}

func newVNC(addr, password string) *vncClient {
	v := &vncClient{addr: addr, password: password}
	go v.run()
	return v
}

// run тримає з'єднання: після розриву підключається знову.
func (v *vncClient) run() {
	for {
		err := v.session()
		v.mu.Lock()
		v.lastErr, v.ready, v.conn = err, false, nil
		v.mu.Unlock()
		logf("VNC: %v; повторне підключення за 2 с", err)
		time.Sleep(2 * time.Second)
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

func (v *vncClient) session() error {
	conn, err := net.DialTimeout("tcp", v.addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}
	r := bufio.NewReaderSize(conn, 1<<16)
	rd := func(n int) ([]byte, error) {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		return b, err
	}
	reason := func() string {
		if l, err := rd(4); err == nil {
			if s, err := rd(int(min(binary.BigEndian.Uint32(l), 1024))); err == nil {
				return string(s)
			}
		}
		return "причина невідома"
	}

	conn.SetDeadline(time.Now().Add(20 * time.Second))
	ver, err := rd(12)
	if err != nil {
		return err
	}
	if string(ver[:4]) != "RFB " {
		return errors.New("це не VNC-сервер")
	}
	minor := int(ver[8]-'0')*100 + int(ver[9]-'0')*10 + int(ver[10]-'0')
	var sec byte
	if minor >= 7 {
		conn.Write([]byte("RFB 003.008\n"))
		n, err := rd(1)
		if err != nil {
			return err
		}
		if n[0] == 0 {
			return fmt.Errorf("сервер відмовив: %s", reason())
		}
		types, err := rd(int(n[0]))
		if err != nil {
			return err
		}
		switch {
		case bytes.IndexByte(types, 2) >= 0 && v.password != "":
			sec = 2
		case bytes.IndexByte(types, 1) >= 0:
			sec = 1
		case bytes.IndexByte(types, 2) >= 0:
			return errors.New("сервер вимагає пароль, а його не задано")
		default:
			return fmt.Errorf("сервер не пропонує підтримуваного захисту (%v)", types)
		}
		conn.Write([]byte{sec})
	} else {
		conn.Write([]byte("RFB 003.003\n"))
		t, err := rd(4)
		if err != nil {
			return err
		}
		if sec = t[3]; sec == 0 {
			return fmt.Errorf("сервер відмовив: %s", reason())
		}
	}
	if sec == 2 {
		ch, err := rd(16)
		if err != nil {
			return err
		}
		conn.Write(vncResponse(v.password, ch))
	}
	if sec == 2 || minor >= 8 {
		res, err := rd(4)
		if err != nil {
			return err
		}
		if binary.BigEndian.Uint32(res) != 0 {
			if minor >= 8 {
				return fmt.Errorf("сервер не прийняв пароль: %s", reason())
			}
			return errors.New("сервер не прийняв пароль")
		}
	}
	conn.Write([]byte{1}) // ClientInit: спільний сеанс, інших клієнтів не від'єднувати
	si, err := rd(24)
	if err != nil {
		return err
	}
	w, h := int(binary.BigEndian.Uint16(si[0:])), int(binary.BigEndian.Uint16(si[2:]))
	nameBytes, err := rd(int(min(binary.BigEndian.Uint32(si[20:]), 4096)))
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Time{})

	// 32 біти, порядок байтів у пам'яті R, G, B, X — одразу придатний як RGBA
	conn.Write([]byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0})
	encs := []int32{encZRLE, encRaw, encDesktopSize, encExactKeys}
	se := []byte{2, 0, 0, byte(len(encs))}
	for _, e := range encs {
		se = binary.BigEndian.AppendUint32(se, uint32(e))
	}
	conn.Write(se)

	v.mu.Lock()
	v.conn, v.name, v.lastErr = conn, string(nameBytes), nil
	v.resize(w, h)
	v.mu.Unlock()
	logf("VNC: підключено до %s («%s», %dx%d)", v.addr, nameBytes, w, h)

	request := func(incremental byte) {
		v.mu.Lock()
		m := []byte{3, incremental, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(m[6:], uint16(v.w))
		binary.BigEndian.PutUint16(m[8:], uint16(v.h))
		v.mu.Unlock()
		v.send(m)
	}
	request(0)

	z := &zstream{}
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0: // FramebufferUpdate
			hd, err := rd(3)
			if err != nil {
				return err
			}
			resized := false
			for i := int(binary.BigEndian.Uint16(hd[1:])); i > 0; i-- {
				rh, err := rd(12)
				if err != nil {
					return err
				}
				x, y := int(binary.BigEndian.Uint16(rh[0:])), int(binary.BigEndian.Uint16(rh[2:]))
				rw, rhh := int(binary.BigEndian.Uint16(rh[4:])), int(binary.BigEndian.Uint16(rh[6:]))
				switch enc := int32(binary.BigEndian.Uint32(rh[8:])); enc {
				case encDesktopSize:
					v.mu.Lock()
					v.resize(rw, rhh)
					v.ready = false
					v.mu.Unlock()
					resized = true
				case encRaw:
					data, err := rd(rw * rhh * 4)
					if err != nil {
						return err
					}
					v.mu.Lock()
					if v.inside(x, y, rw, rhh) {
						for row := 0; row < rhh; row++ {
							copy(v.fb[((y+row)*v.w+x)*4:], data[row*rw*4:(row+1)*rw*4])
						}
					}
					v.mu.Unlock()
				case encZRLE:
					l, err := rd(4)
					if err != nil {
						return err
					}
					data, err := rd(int(binary.BigEndian.Uint32(l)))
					if err != nil {
						return err
					}
					v.mu.Lock()
					ok := v.inside(x, y, rw, rhh)
					if ok {
						err = z.decode(data, v.fb, v.w, x, y, rw, rhh)
					}
					v.mu.Unlock()
					if !ok {
						return errors.New("прямокутник поза екраном")
					}
					if err != nil {
						return err
					}
				default:
					return fmt.Errorf("невідоме кодування %d", enc)
				}
			}
			v.mu.Lock()
			v.lastUpdate = time.Now()
			if !resized {
				v.ready = true
			}
			v.mu.Unlock()
			if resized {
				request(0) // після зміни розміру потрібен повний кадр
			} else {
				request(1)
			}
		case 1: // SetColourMapEntries
			hd, err := rd(5)
			if err != nil {
				return err
			}
			if _, err := rd(int(binary.BigEndian.Uint16(hd[3:])) * 6); err != nil {
				return err
			}
		case 2: // Bell
		case 3: // ServerCutText (звичайний або розширений — довжина може бути від'ємною)
			hd, err := rd(7)
			if err != nil {
				return err
			}
			n := int32(binary.BigEndian.Uint32(hd[3:]))
			if n < 0 {
				n = -n
			}
			if _, err := r.Discard(int(n)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("невідоме повідомлення сервера %d", t)
		}
	}
}

// resize і inside викликаються з утримуваним mu.
func (v *vncClient) resize(w, h int) {
	v.w, v.h = w, h
	v.fb = make([]byte, w*h*4)
}

func (v *vncClient) inside(x, y, w, h int) bool {
	return x >= 0 && y >= 0 && w >= 0 && h >= 0 && x+w <= v.w && y+h <= v.h
}

func (v *vncClient) send(m []byte) error {
	v.mu.Lock()
	conn := v.conn
	v.mu.Unlock()
	if conn == nil {
		return errors.New("немає з'єднання з VNC-сервером")
	}
	v.wmu.Lock()
	defer v.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write(m)
	return err
}

func (v *vncClient) pointer(x, y int, mask byte) error {
	return v.send([]byte{5, mask, byte(x >> 8), byte(x), byte(y >> 8), byte(y)})
}

func (v *vncClient) key(sym uint32, down bool) error {
	m := []byte{4, 0, 0, 0, byte(sym >> 24), byte(sym >> 16), byte(sym >> 8), byte(sym)}
	if down {
		m[1] = 1
	}
	return v.send(m)
}

// snapshot чекає на готовий кадр і повертає його копію.
func (v *vncClient) snapshot(wait time.Duration) (fb []byte, w, h int, err error) {
	deadline := time.Now().Add(wait)
	for {
		v.mu.Lock()
		if v.ready {
			fb = append([]byte(nil), v.fb...)
			w, h = v.w, v.h
			v.mu.Unlock()
			return fb, w, h, nil
		}
		last := v.lastErr
		v.mu.Unlock()
		if time.Now().After(deadline) {
			if last != nil {
				return nil, 0, 0, fmt.Errorf("cannot reach the VNC server at %s: %v", v.addr, last)
			}
			return nil, 0, 0, fmt.Errorf("no frame from the VNC server at %s yet", v.addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// settle чекає, доки екран перестане змінюватися після дії (але не довше max).
func (v *vncClient) settle(first, quiet, max time.Duration) {
	start := time.Now()
	time.Sleep(first)
	for time.Since(start) < max {
		v.mu.Lock()
		idle := time.Since(v.lastUpdate)
		v.mu.Unlock()
		if idle >= quiet {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (v *vncClient) size() (int, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.w, v.h
}

// zstream — безперервний zlib-потік ZRLE: один на все з'єднання.
type zstream struct {
	in  bytes.Buffer
	r   io.Reader
	buf []byte
}

func (z *zstream) read(n int) ([]byte, error) {
	if cap(z.buf) < n {
		z.buf = make([]byte, n)
	}
	b := z.buf[:n]
	_, err := io.ReadFull(z.r, b)
	return b, err
}

// runLength читає довжину серії: байти підсумовуються, 255 означає «далі ще».
func (z *zstream) runLength() (int, error) {
	n := 1
	for {
		b, err := z.read(1)
		if err != nil {
			return 0, err
		}
		n += int(b[0])
		if b[0] != 255 {
			return n, nil
		}
	}
}

// decode розпаковує прямокутник ZRLE у fb (RGBA, ширина рядка fbW пікселів).
func (z *zstream) decode(data, fb []byte, fbW, x, y, w, h int) error {
	z.in.Write(data)
	if z.r == nil {
		r, err := zlib.NewReader(&z.in)
		if err != nil {
			return err
		}
		z.r = r
	}
	put := func(px, py int, c []byte) {
		o := (py*fbW + px) * 4
		fb[o], fb[o+1], fb[o+2], fb[o+3] = c[0], c[1], c[2], 255
	}
	var pal [127 * 3]byte
	for ty := 0; ty < h; ty += 64 {
		th := min(64, h-ty)
		for tx := 0; tx < w; tx += 64 {
			tw := min(64, w-tx)
			s, err := z.read(1)
			if err != nil {
				return err
			}
			sub := int(s[0])
			switch {
			case sub == 0: // сирі пікселі
				d, err := z.read(tw * th * 3)
				if err != nil {
					return err
				}
				for j := 0; j < tw*th; j++ {
					put(x+tx+j%tw, y+ty+j/tw, d[j*3:])
				}
			case sub == 1: // суцільний колір
				d, err := z.read(3)
				if err != nil {
					return err
				}
				c := [3]byte{d[0], d[1], d[2]}
				for j := 0; j < tw*th; j++ {
					put(x+tx+j%tw, y+ty+j/tw, c[:])
				}
			case sub <= 16: // упакована палітра
				d, err := z.read(sub * 3)
				if err != nil {
					return err
				}
				copy(pal[:], d)
				bpp := 4
				if sub == 2 {
					bpp = 1
				} else if sub <= 4 {
					bpp = 2
				}
				rowBytes := (tw*bpp + 7) / 8
				d, err = z.read(rowBytes * th)
				if err != nil {
					return err
				}
				for yy := 0; yy < th; yy++ {
					for xx := 0; xx < tw; xx++ {
						bit := xx * bpp
						i := int(d[yy*rowBytes+bit/8]>>(8-bpp-bit%8)) & (1<<bpp - 1)
						if i >= sub {
							return errors.New("ZRLE: індекс поза палітрою")
						}
						put(x+tx+xx, y+ty+yy, pal[i*3:])
					}
				}
			case sub == 128: // серії кольорів
				for j := 0; j < tw*th; {
					d, err := z.read(3)
					if err != nil {
						return err
					}
					c := [3]byte{d[0], d[1], d[2]}
					n, err := z.runLength()
					if err != nil {
						return err
					}
					if j+n > tw*th {
						return errors.New("ZRLE: серія виходить за плитку")
					}
					for ; n > 0; n, j = n-1, j+1 {
						put(x+tx+j%tw, y+ty+j/tw, c[:])
					}
				}
			case sub >= 130: // серії з палітрою
				size := sub - 128
				d, err := z.read(size * 3)
				if err != nil {
					return err
				}
				copy(pal[:], d)
				for j := 0; j < tw*th; {
					b, err := z.read(1)
					if err != nil {
						return err
					}
					i, n := int(b[0]&127), 1
					if b[0]&128 != 0 {
						if n, err = z.runLength(); err != nil {
							return err
						}
					}
					if i >= size || j+n > tw*th {
						return errors.New("ZRLE: пошкоджена серія з палітрою")
					}
					for ; n > 0; n, j = n-1, j+1 {
						put(x+tx+j%tw, y+ty+j/tw, pal[i*3:])
					}
				}
			default:
				return fmt.Errorf("ZRLE: невідомий тип плитки %d", sub)
			}
		}
	}
	return nil
}
