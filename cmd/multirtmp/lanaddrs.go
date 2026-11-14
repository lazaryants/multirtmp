package main

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// adapterInfo — то, что нужно от сетевого адаптера для выбора адреса для
// ссылки на планшет. Отдельный тип (а не сразу windows.IpAdapterAddresses),
// чтобы отбор (rankLANIPs) был чистой функцией и тестировался без реальных
// адаптеров.
type adapterInfo struct {
	name     string
	up       bool
	ifType   uint32
	addrs    []net.IPNet // только IPv4, с длиной префикса
	gateways []net.IP    // шлюзы, прописанные у ЭТОГО адаптера (IPv4)
}

// minTunnelPrefix — маска /30 и уже (/31, /32) — это туннели точка-точка
// (VPN TUN-адаптеры вроде Karing с 10.20.0.1/30), а не сеть, в которой может
// оказаться планшет.
const minTunnelPrefix = 30

// rankLANIPs — IPv4-адреса, по которым эту машину может найти устройство в
// локальной сети.
//
// Отсеиваются: выключенные адаптеры, loopback, туннели и PPP (point-to-point
// по типу интерфейса), адреса с маской /30 и уже (TUN-адаптеры VPN часто
// имеют тип "виртуальный", а не "туннель", — их ловит маска), link-local
// 169.254.x.x.
//
// Если есть адреса, в подсети которых лежит шлюз их же адаптера, —
// возвращаются ТОЛЬКО они: это сеть, через которую машина реально выходит
// наружу, и в ней же планшет. Остальные адреса на том же адаптере на
// площадке — статические, для настройки роутеров, с планшета не нужны.
// Шлюз берётся из настроек адаптера, а не из таблицы маршрутов: VPN в
// режиме TUN может забирать маршрут по умолчанию себе.
//
// Полный список (в порядке, в котором его отдала система) — только если ни
// у одного адаптера нет шлюза или ни один адрес не попал в подсеть шлюза:
// тогда угадывать не из чего, и оператор выбирает сам.
func rankLANIPs(adapters []adapterInfo) []string {
	var primary, rest []string
	seen := map[string]bool{}

	for _, a := range adapters {
		if !a.up {
			continue
		}
		switch a.ifType {
		case windows.IF_TYPE_SOFTWARE_LOOPBACK, windows.IF_TYPE_TUNNEL, windows.IF_TYPE_PPP:
			continue
		}

		for _, n := range a.addrs {
			ip4 := n.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
				continue
			}
			if ones, _ := n.Mask.Size(); ones >= minTunnelPrefix {
				continue
			}

			s := ip4.String()
			if seen[s] {
				continue
			}
			seen[s] = true

			if subnetHasGateway(n, a.gateways) {
				primary = append(primary, s)
			} else {
				rest = append(rest, s)
			}
		}
	}

	if len(primary) > 0 {
		return primary
	}
	return rest
}

func subnetHasGateway(n net.IPNet, gateways []net.IP) bool {
	for _, gw := range gateways {
		if n.Contains(gw) {
			return true
		}
	}
	return false
}

// readAdapters — IPv4-адреса и шлюзы всех адаптеров через
// GetAdaptersAddresses: в отличие от net.Interfaces, отдаёт и тип
// интерфейса, и шлюз каждого адаптера.
func readAdapters() ([]adapterInfo, error) {
	const flags = windows.GAA_FLAG_INCLUDE_GATEWAYS | windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER

	size := uint32(15 * 1024)
	var buf []byte
	for i := 0; i < 3; i++ {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW || i == 2 {
			return nil, fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
	}

	var out []adapterInfo
	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		a := adapterInfo{
			name:   windows.UTF16PtrToString(aa.FriendlyName),
			up:     aa.OperStatus == windows.IfOperStatusUp,
			ifType: aa.IfType,
		}
		for u := aa.FirstUnicastAddress; u != nil; u = u.Next {
			if ip := u.Address.IP().To4(); ip != nil {
				a.addrs = append(a.addrs, net.IPNet{IP: ip, Mask: net.CIDRMask(int(u.OnLinkPrefixLength), 32)})
			}
		}
		for g := aa.FirstGatewayAddress; g != nil; g = g.Next {
			if ip := g.Address.IP().To4(); ip != nil {
				a.gateways = append(a.gateways, ip)
			}
		}
		out = append(out, a)
	}

	return out, nil
}
