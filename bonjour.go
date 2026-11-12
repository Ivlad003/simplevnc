//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Оголошення в локальній мережі (Bonjour / mDNS, служба _rfb._tcp): Mac сам
// показує цей ПК у Finder → «Мережа», і підключення йде на правильний порт без
// введення адреси. Використовує вбудований у Windows 10+ відповідач mDNS.

var (
	dnsapi                       = syscall.NewLazyDLL("dnsapi.dll")
	pDnsServiceConstructInstance = dnsapi.NewProc("DnsServiceConstructInstance")
	pDnsServiceRegister          = dnsapi.NewProc("DnsServiceRegister")

	bonjourReq  *dnsServiceRegisterRequest // тримаємо живим: реєстрація асинхронна
	bonjourName string
	bonjourDone = syscall.NewCallback(func(status, ctx, instance uintptr) uintptr {
		if status == 0 {
			logf("Bonjour: ПК оголошено в локальній мережі як «%s»", bonjourName)
		} else {
			logf("Bonjour: оголосити ПК в мережі не вдалося (код %d)", status)
		}
		return 0
	})
)

type dnsServiceRegisterRequest struct {
	version        uint32
	interfaceIndex uint32
	instance       uintptr
	callback       uintptr
	context        uintptr
	credentials    uintptr
	unicastEnabled int32
}

const dnsRequestPending = 9506

func advertiseBonjour(host string, port int) {
	if pDnsServiceRegister.Find() != nil || pDnsServiceConstructInstance.Find() != nil {
		return // стара Windows без вбудованого mDNS
	}
	name, err1 := syscall.UTF16PtrFromString(host + "._rfb._tcp.local")
	target, err2 := syscall.UTF16PtrFromString(host + ".local")
	if err1 != nil || err2 != nil {
		return
	}
	inst, _, _ := pDnsServiceConstructInstance.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(target)),
		0, 0, uintptr(port), 0, 0, 0, 0, 0)
	if inst == 0 {
		return
	}
	bonjourName = host
	bonjourReq = &dnsServiceRegisterRequest{version: 1, instance: inst, callback: bonjourDone}
	if r, _, _ := pDnsServiceRegister.Call(uintptr(unsafe.Pointer(bonjourReq)), 0); r != dnsRequestPending {
		logf("Bonjour: оголосити ПК в мережі не вдалося (код %d)", r)
	}
}
