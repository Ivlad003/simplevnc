//go:build windows

package main

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// Порт IPv4 тримає інша програма: спільний сокет IPv4+IPv6 Windows усе одно відкриває,
// і клієнти IPv4 потрапляють до чужої програми. listenTCP має це помітити.
func TestListenTCPSeesBusyIPv4Port(t *testing.T) {
	other, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	port := other.Addr().(*net.TCPAddr).Port

	if ln, err := listenTCP("", port); err == nil {
		ln.Close()
		t.Fatalf("порт %d зайнятий для IPv4, а listenTCP його відкрив", port)
	}
	if ln, err := net.Listen("tcp", ":"+strconv.Itoa(port)); err == nil {
		ln.Close()
		t.Logf("звичайний net.Listen порт %d відкрив — саме від цього й захищаємось", port)
	}
}

func TestListenTCPAcceptsBothFamilies(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0") // беремо вільний номер порту
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	ln, err := listenTCP("", port)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
		if err != nil {
			if host == "::1" {
				t.Logf("IPv6 недоступний: %v", err)
				continue
			}
			t.Fatalf("%s: %v", host, err)
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 2)
		if _, err := c.Read(b); err != nil || string(b) != "ok" {
			t.Fatalf("%s: відповідь %q, помилка %v", host, b, err)
		}
		c.Close()
	}

	// задана адреса — слухаємо лише її
	one, err := listenTCP("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	one.Close()
}
