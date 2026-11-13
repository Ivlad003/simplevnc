//go:build windows

package main

import (
	"net"
	"strconv"
	"sync"
	"time"
)

// listenTCP відкриває порт. Без заданої адреси слухаємо окремо IPv4 та IPv6:
// спільний сокет Windows дає відкрити, навіть коли порт IPv4 уже тримає інша
// програма (наприклад, інший VNC-сервер на 5900) — і тоді клієнти потрапляють до неї,
// а не до нас. IPv4 обов'язковий, IPv6 — якщо вдасться.
func listenTCP(bind string, port int) (net.Listener, error) {
	p := strconv.Itoa(port)
	if bind != "" {
		return net.Listen("tcp", net.JoinHostPort(bind, p))
	}
	l4, err := net.Listen("tcp4", "0.0.0.0:"+p)
	if err != nil {
		return nil, err
	}
	l6, err := net.Listen("tcp6", "[::]:"+p)
	if err != nil {
		return l4, nil
	}
	return newMultiListener(l4, l6), nil
}

// multiListener приймає з'єднання з кількох портів як один.
type multiListener struct {
	ls   []net.Listener
	conn chan net.Conn
	done chan struct{}
	once sync.Once
}

func newMultiListener(ls ...net.Listener) net.Listener {
	m := &multiListener{ls: ls, conn: make(chan net.Conn), done: make(chan struct{})}
	for _, l := range ls {
		go func(l net.Listener) {
			for {
				c, err := l.Accept()
				if err != nil {
					select {
					case <-m.done:
						return
					case <-time.After(100 * time.Millisecond): // тимчасова помилка — пробуємо далі
						continue
					}
				}
				select {
				case m.conn <- c:
				case <-m.done:
					c.Close()
					return
				}
			}
		}(l)
	}
	return m
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case c := <-m.conn:
		return c, nil
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	m.once.Do(func() {
		close(m.done)
		for _, l := range m.ls {
			l.Close()
		}
	})
	return nil
}

func (m *multiListener) Addr() net.Addr { return m.ls[0].Addr() }
