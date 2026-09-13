package cmd

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestParseDirectMode(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		env  string
		want string
	}{
		{name: "default is auto", flag: false, env: "", want: directAuto},
		{name: "--direct forces always", flag: true, env: "", want: directAlways},
		{name: "--direct wins over env never", flag: true, env: "never", want: directAlways},
		{name: "env always", flag: false, env: "always", want: directAlways},
		{name: "env never", flag: false, env: "never", want: directNever},
		{name: "env auto", flag: false, env: "auto", want: directAuto},
		{name: "env 1 means always", flag: false, env: "1", want: directAlways},
		{name: "env true means always", flag: false, env: "true", want: directAlways},
		{name: "env 0 means never", flag: false, env: "0", want: directNever},
		{name: "env false means never", flag: false, env: "false", want: directNever},
		{name: "garbage falls back to auto", flag: false, env: "yes-please", want: directAuto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDirectMode(tc.flag, tc.env); got != tc.want {
				t.Errorf("parseDirectMode(%v, %q) = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// Fixtures mimic `ip -j route show` output (iproute2 JSON, main table).
const (
	fibDefaultOnly = `[
  {"dst":"default","gateway":"192.168.1.1","dev":"wlan0","protocol":"dhcp","prefsrc":"192.168.1.23","metric":600,"flags":[]},
  {"dst":"192.168.1.0/24","dev":"wlan0","protocol":"kernel","scope":"link","prefsrc":"192.168.1.23","metric":600,"flags":[]}
]`
	fibRoutedLab = `[
  {"dst":"default","gateway":"192.168.1.1","dev":"wlan0","protocol":"dhcp","flags":[]},
  {"dst":"10.233.192.0/24","gateway":"172.16.0.1","dev":"eth1","protocol":"static","flags":[]},
  {"dst":"192.168.1.0/24","dev":"wlan0","protocol":"kernel","scope":"link","flags":[]}
]`
	fibOnNode = `[
  {"dst":"default","gateway":"192.168.1.1","dev":"eth0","flags":[]},
  {"dst":"10.233.192.242","dev":"cali7a3f10e2c41","scope":"link","flags":[]},
  {"dst":"10.233.192.0/26","dev":"vxlan.calico","onlink":true,"gateway":"10.233.192.192","flags":[]}
]`
	fibBlackhole = `[
  {"dst":"default","gateway":"192.168.1.1","dev":"eth0","flags":[]},
  {"type":"blackhole","dst":"10.233.0.0/16","flags":[]}
]`
	// LPM: the /16 would allow it, but a more specific blackhole /24 wins.
	fibBlackholeMoreSpecific = `[
  {"dst":"10.233.0.0/16","gateway":"172.16.0.1","dev":"eth1","flags":[]},
  {"type":"blackhole","dst":"10.233.192.0/24","flags":[]}
]`
	// LPM the other way round: broad blackhole, specific unicast carve-out.
	fibUnicastMoreSpecific = `[
  {"type":"blackhole","dst":"10.233.0.0/16","flags":[]},
  {"dst":"10.233.192.0/24","gateway":"172.16.0.1","dev":"eth1","flags":[]}
]`
	fibV6 = `[
  {"dst":"fd00:10:233::/64","gateway":"fe80::1","dev":"eth1","protocol":"static","flags":[]},
  {"dst":"default","gateway":"fe80::1","dev":"eth0","flags":[]}
]`
)

func TestRouteCovers(t *testing.T) {
	cases := []struct {
		name    string
		routes  string
		ip      string
		want    bool
		wantVia string // expected covering dst ("" when want=false)
		wantErr bool
	}{
		{name: "default route only", routes: fibDefaultOnly, ip: "10.233.192.53", want: false},
		{name: "specific static route covers", routes: fibRoutedLab, ip: "10.233.192.53", want: true, wantVia: "10.233.192.0/24"},
		{name: "specific route does not cover other subnet", routes: fibRoutedLab, ip: "10.234.0.5", want: false},
		{name: "on-node host route covers exactly", routes: fibOnNode, ip: "10.233.192.242", want: true, wantVia: "10.233.192.242"},
		{name: "blackhole is not coverage", routes: fibBlackhole, ip: "10.233.192.53", want: false},
		{name: "more specific blackhole beats broad unicast", routes: fibBlackholeMoreSpecific, ip: "10.233.192.53", want: false},
		{name: "more specific unicast beats broad blackhole", routes: fibUnicastMoreSpecific, ip: "10.233.192.53", want: true, wantVia: "10.233.192.0/24"},
		{name: "v6 route covers v6 pod IP", routes: fibV6, ip: "fd00:10:233::53", want: true, wantVia: "fd00:10:233::/64"},
		{name: "invalid JSON errors", routes: "not json", ip: "10.0.0.1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			covered, via, err := routeCovers([]byte(tc.routes), netip.MustParseAddr(tc.ip))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("routeCovers() err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("routeCovers() unexpected error: %v", err)
			}
			if covered != tc.want || via != tc.wantVia {
				t.Errorf("routeCovers(%s) = (%v, %q), want (%v, %q)", tc.ip, covered, via, tc.want, tc.wantVia)
			}
		})
	}
}

func TestDirectProberProbe(t *testing.T) {
	dialOK := func(string) error { return nil }
	dialFail := func(string) error { return errors.New("connection refused") }
	dialBoom := func(string) error {
		panic("dial must not be called")
	}
	routesLab := func(bool) ([]byte, error) { return []byte(fibRoutedLab), nil }
	routesDefault := func(bool) ([]byte, error) { return []byte(fibDefaultOnly), nil }
	routesErr := func(bool) ([]byte, error) { return nil, errors.New("ip: not found") }
	routesBoom := func(bool) ([]byte, error) {
		panic("listRoutes must not be called")
	}

	cases := []struct {
		name       string
		mode       string
		listRoutes func(bool) ([]byte, error)
		dial       func(string) error
		ip         string
		want       bool
		wantReason string // substring the reason must contain
	}{
		{
			name: "never: nothing consulted", mode: directNever,
			listRoutes: routesBoom, dial: dialBoom,
			ip: "10.233.192.53", want: false, wantReason: "disabled",
		},
		{
			name: "auto: covered and dial ok", mode: directAuto,
			listRoutes: routesLab, dial: dialOK,
			ip: "10.233.192.53", want: true, wantReason: "10.233.192.0/24",
		},
		{
			name: "auto: covered but dial fails", mode: directAuto,
			listRoutes: routesLab, dial: dialFail,
			ip: "10.233.192.53", want: false, wantReason: "connection refused",
		},
		{
			name: "auto: not covered, dial never attempted", mode: directAuto,
			listRoutes: routesDefault, dial: dialBoom,
			ip: "10.233.192.53", want: false, wantReason: "no specific route",
		},
		{
			name: "auto: route listing fails, dial never attempted", mode: directAuto,
			listRoutes: routesErr, dial: dialBoom,
			ip: "10.233.192.53", want: false, wantReason: "ip: not found",
		},
		{
			name: "always: route table not consulted, dial ok", mode: directAlways,
			listRoutes: routesBoom, dial: dialOK,
			ip: "10.233.192.53", want: true, wantReason: "forced",
		},
		{
			name: "always: dial fails", mode: directAlways,
			listRoutes: routesBoom, dial: dialFail,
			ip: "10.233.192.53", want: false, wantReason: "connection refused",
		},
		{
			name: "invalid pod IP", mode: directAuto,
			listRoutes: routesBoom, dial: dialBoom,
			ip: "", want: false, wantReason: "no pod IP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &directProber{mode: tc.mode, listRoutes: tc.listRoutes, dial: tc.dial}
			got, reason := p.probe(tc.ip, 1024)
			if got != tc.want {
				t.Errorf("probe(%q) = %v, want %v (reason: %s)", tc.ip, got, tc.want, reason)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("probe(%q) reason = %q, want it to contain %q", tc.ip, reason, tc.wantReason)
			}
		})
	}
}
