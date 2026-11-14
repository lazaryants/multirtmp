package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"multirtmp/internal/config"
)

// lanHostPlaceholder — подставляется вместо адреса, если ни одного
// подходящего не нашлось: оператор видит формат ссылки и заменит адрес сам
// (или задаст api.public_host).
const lanHostPlaceholder = "<this-machine-LAN-IP>"

// detectLANURLs — ссылки на веб-интерфейс для устройств в локальной сети
// (планшет и т.п.), лучшая первой. Используются и в стартовом логе, и в поле
// network.lan_urls для кнопки "Show link for tablet". Пусто, если api.addr —
// loopback (LAN-доступа нет вообще).
func detectLANURLs(apiAddr, token, publicHost string) ([]string, error) {
	return buildLANURLs(apiAddr, token, publicHost, func() ([]string, error) {
		adapters, err := readAdapters()
		if err != nil {
			return nil, err
		}
		return rankLANIPs(adapters), nil
	})
}

// buildLANURLs — логика detectLANURLs без обращения к реальным адаптерам
// (candidates подставляется в тестах):
//
//   - api.addr loopback — ссылок нет;
//   - задан api.public_host — ровно одна ссылка с ним, без угадывания;
//   - api.addr привязан к конкретному адресу — ровно одна ссылка на него
//     (сервер слушает только его, другие адреса бесполезны);
//   - api.addr слушает все интерфейсы — по ссылке на каждый адрес из
//     candidates (уже отсортированы, лучший первым); если их нет —
//     одна ссылка с заглушкой вместо адреса.
//
// Ошибка возвращается только из candidates — при этом ссылка с заглушкой
// всё равно отдаётся, чтобы кнопка в интерфейсе не пропадала.
func buildLANURLs(apiAddr, token, publicHost string, candidates func() ([]string, error)) ([]string, error) {
	if config.IsLoopbackAddr(apiAddr) {
		return nil, nil
	}

	host, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		return nil, nil
	}

	mk := func(h string) string {
		u := "http://" + net.JoinHostPort(h, port) + "/"
		if token != "" {
			u += "?token=" + url.QueryEscape(token)
		}
		return u
	}

	if publicHost != "" {
		return []string{mk(strings.TrimSuffix(strings.TrimPrefix(publicHost, "["), "]"))}, nil
	}

	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{mk(host)}, nil
	}

	ips, err := candidates()
	if err != nil {
		return []string{mk(lanHostPlaceholder)}, fmt.Errorf("failed to list network adapters: %w", err)
	}
	if len(ips) == 0 {
		return []string{mk(lanHostPlaceholder)}, nil
	}

	urls := make([]string, len(ips))
	for i, ip := range ips {
		urls[i] = mk(ip)
	}
	return urls, nil
}

// portOf возвращает порт из host:port, пустую строку при ошибке разбора
// (addr уже прошёл валидацию в config.resolve, так что на практике не
// случается).
func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}
