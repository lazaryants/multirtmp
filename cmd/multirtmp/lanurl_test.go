package main

import (
	"errors"
	"net"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func ipnet(s string) net.IPNet {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return net.IPNet{IP: ip, Mask: n.Mask}
}

// Раскладка машины из отчёта: VPN Karing (TUN, /30, без шлюза) и Ethernet с
// тремя адресами, шлюз 192.168.1.1. Дополнительные 192.168.10.12 и
// 192.168.131.12 — статические, для настройки роутеров: раз есть адрес в
// подсети шлюза, остаётся только он, хотя Ethernet отдаёт его последним.
func TestRankLANIPsVPNMachine(t *testing.T) {
	adapters := []adapterInfo{
		{name: "Karing TUN Network Adapter", up: true, ifType: 53, addrs: []net.IPNet{ipnet("10.20.0.1/30")}},
		{name: "Ethernet", up: true, ifType: 6,
			addrs:    []net.IPNet{ipnet("192.168.131.12/24"), ipnet("192.168.10.12/24"), ipnet("192.168.1.12/24")},
			gateways: []net.IP{net.ParseIP("192.168.1.1")}},
		{name: "Loopback Pseudo-Interface 1", up: true, ifType: windows.IF_TYPE_SOFTWARE_LOOPBACK, addrs: []net.IPNet{ipnet("127.0.0.1/8")}},
	}

	got := rankLANIPs(adapters)
	want := []string{"192.168.1.12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Два адаптера, у каждого свой шлюз (например, Ethernet и Wi-Fi в разных
// сетях): оба адреса в подсети шлюза — показываются оба, а статический адрес
// на Ethernet — нет.
func TestRankLANIPsTwoGatewayNetworks(t *testing.T) {
	adapters := []adapterInfo{
		{name: "Ethernet", up: true, ifType: 6,
			addrs:    []net.IPNet{ipnet("192.168.10.12/24"), ipnet("192.168.1.12/24")},
			gateways: []net.IP{net.ParseIP("192.168.1.1")}},
		{name: "Wi-Fi", up: true, ifType: 71,
			addrs:    []net.IPNet{ipnet("10.0.5.40/24")},
			gateways: []net.IP{net.ParseIP("10.0.5.1")}},
	}

	got := rankLANIPs(adapters)
	want := []string{"192.168.1.12", "10.0.5.40"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Полный список — только когда опереться не на что: шлюзов нет вовсе или
// ни один адрес не попал в подсеть шлюза.
func TestRankLANIPsFallbackFullList(t *testing.T) {
	cases := []struct {
		name     string
		adapters []adapterInfo
		want     []string
	}{
		{"no gateways at all", []adapterInfo{
			{name: "Ethernet", up: true, ifType: 6, addrs: []net.IPNet{ipnet("192.168.131.12/24"), ipnet("192.168.1.12/24")}},
			{name: "Wi-Fi", up: true, ifType: 71, addrs: []net.IPNet{ipnet("10.0.5.40/24")}},
		}, []string{"192.168.131.12", "192.168.1.12", "10.0.5.40"}},
		{"gateway outside every subnet", []adapterInfo{
			{name: "Ethernet", up: true, ifType: 6,
				addrs:    []net.IPNet{ipnet("192.168.131.12/24"), ipnet("192.168.1.12/24")},
				gateways: []net.IP{net.ParseIP("172.20.0.1")}},
		}, []string{"192.168.131.12", "192.168.1.12"}},
		// Шлюз есть только у VPN, но сама VPN отсеяна по маске — он не
		// должен ни выбрать VPN, ни "съесть" остальные адреса.
		{"gateway only on a filtered VPN", []adapterInfo{
			{name: "vpn", up: true, ifType: 53, addrs: []net.IPNet{ipnet("10.8.0.6/30")}, gateways: []net.IP{net.ParseIP("10.8.0.5")}},
			{name: "Ethernet", up: true, ifType: 6, addrs: []net.IPNet{ipnet("192.168.1.12/24")}},
		}, []string{"192.168.1.12"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rankLANIPs(tc.adapters)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRankLANIPsFilters(t *testing.T) {
	adapters := []adapterInfo{
		{name: "down", up: false, ifType: 6, addrs: []net.IPNet{ipnet("192.168.5.5/24")}},
		{name: "tunnel type", up: true, ifType: windows.IF_TYPE_TUNNEL, addrs: []net.IPNet{ipnet("172.16.0.2/24")}},
		{name: "ppp", up: true, ifType: windows.IF_TYPE_PPP, addrs: []net.IPNet{ipnet("172.17.0.2/24")}},
		{name: "slash31", up: true, ifType: 6, addrs: []net.IPNet{ipnet("10.9.0.0/31")}},
		{name: "slash32", up: true, ifType: 6, addrs: []net.IPNet{ipnet("10.9.1.1/32")}},
		{name: "apipa", up: true, ifType: 6, addrs: []net.IPNet{ipnet("169.254.10.10/16")}},
		{name: "wifi", up: true, ifType: 71, addrs: []net.IPNet{ipnet("192.168.0.20/24")}},
		// VPN, которая забрала себе шлюз, но с узкой маской — всё равно отсеивается.
		{name: "vpn with gw", up: true, ifType: 53, addrs: []net.IPNet{ipnet("10.8.0.6/30")}, gateways: []net.IP{net.ParseIP("10.8.0.5")}},
	}

	got := rankLANIPs(adapters)
	want := []string{"192.168.0.20"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildLANURLs(t *testing.T) {
	cands := func() ([]string, error) { return []string{"192.168.1.12", "192.168.10.12"}, nil }
	none := func() ([]string, error) { return nil, nil }
	fail := func() ([]string, error) { return nil, errors.New("boom") }
	mustNotCall := func() ([]string, error) { t.Fatal("candidates must not be called"); return nil, nil }

	cases := []struct {
		name, addr, token, public string
		cands                     func() ([]string, error)
		want                      []string
		wantErr                   bool
	}{
		{"loopback", "127.0.0.1:8080", "", "", mustNotCall, nil, false},
		{"all interfaces", "0.0.0.0:8080", "tok", "", cands,
			[]string{"http://192.168.1.12:8080/?token=tok", "http://192.168.10.12:8080/?token=tok"}, false},
		{"empty host", ":8080", "tok", "", cands,
			[]string{"http://192.168.1.12:8080/?token=tok", "http://192.168.10.12:8080/?token=tok"}, false},
		{"public_host wins", "0.0.0.0:8080", "tok", "studio-pc", mustNotCall,
			[]string{"http://studio-pc:8080/?token=tok"}, false},
		{"public_host ipv6", "[::]:8080", "tok", "[fd00::5]", mustNotCall,
			[]string{"http://[fd00::5]:8080/?token=tok"}, false},
		{"specific addr", "192.168.10.12:8080", "tok", "", mustNotCall,
			[]string{"http://192.168.10.12:8080/?token=tok"}, false},
		{"no candidates", "0.0.0.0:8080", "tok", "", none,
			[]string{"http://" + lanHostPlaceholder + ":8080/?token=tok"}, false},
		{"adapter error", "0.0.0.0:8080", "tok", "", fail,
			[]string{"http://" + lanHostPlaceholder + ":8080/?token=tok"}, true},
		{"token escaped", "0.0.0.0:8080", "a+b", "", func() ([]string, error) { return []string{"10.0.0.2"}, nil },
			[]string{"http://10.0.0.2:8080/?token=a%2Bb"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLANURLs(tc.addr, tc.token, tc.public, tc.cands)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
