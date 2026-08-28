package main

import (
	"fmt"
	"net"

	"multirtmp/internal/config"
)

// detectLANURL строит ссылку на веб-интерфейс для устройств в локальной
// сети (планшет и т.п.) — используется и в стартовом логе, и в поле
// network.lan_url для кнопки "Show link for tablet" в интерфейсе. Пусто, если
// api.addr — loopback (LAN-доступа нет вообще, ссылка была бы бесполезна).
func detectLANURL(apiAddr, token string) string {
	if config.IsLoopbackAddr(apiAddr) {
		return ""
	}

	host, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		return ""
	}

	displayHost := host
	if host == "" || host == "0.0.0.0" || host == "::" {
		if ip := firstLANIPv4(); ip != "" {
			displayHost = ip
		} else {
			displayHost = "<this-machine-LAN-IP>"
		}
	}

	url := fmt.Sprintf("http://%s:%s/", displayHost, port)
	if token != "" {
		url += "?token=" + token
	}

	return url
}

// firstLANIPv4 — первый найденный "обычный" IPv4-адрес машины (не loopback,
// не link-local 169.254.x.x) — лучшее, что можно сделать без явного указания
// интерфейса оператором. Если интерфейсов несколько (VPN, виртуалки), может
// угадать не тот — тогда оператор просто заменит IP в напечатанной ссылке на
// правильный, значение всё равно лучше, чем пусто.
func firstLANIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}

	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}

	return ""
}

// portOf возвращает порт из host:port, пустую строку при ошибке разбора
// (addr уже прошёл валидацию в config.resolve, так что на практике не
// случается).
func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}
