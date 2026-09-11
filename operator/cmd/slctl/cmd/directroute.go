package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"time"
)

// Direct pod-IP connections: on natively-routed networks (ADR-016 style
// pod-routed labs, or slctl running on a cluster node) the client machine can
// reach pod IPs directly and a port-forward is pure overhead. Whether that
// works is a property of the path between THIS machine and THAT pod network —
// no cluster-side declaration (CR field, annotation, Service) can state it
// truthfully for every client. But the client machine already carries a
// declaration made by whoever configured the routing: a specific (non-default)
// FIB entry covering the pod CIDR. We read that back instead of inventing new
// configuration: if the kernel's main routing table has a specific route
// covering the pod IP, direct connectivity was deliberately set up — confirm
// with one short TCP dial and use it. If only the default route matches, the
// machine was never set up for pod routing — go straight to port-forward, no
// probe, zero added latency for the common case.
//
// Known blind spot (deliberate): a client behind a router that holds the
// pod-CIDR route sees only its default route and gets a (working) port-forward.
// SLCTL_DIRECT_POD_IPS=always / --direct is the escape hatch. Routes in
// non-main tables (policy routing/VPNs) are likewise not consulted.

// Direct pod-IP modes (SLCTL_DIRECT_POD_IPS / --direct).
const (
	directAuto   = "auto"   // FIB route check gates a confirming TCP probe (default)
	directAlways = "always" // skip the route check, probe and use direct
	directNever  = "never"  // never consider direct pod IPs

	directPodIPsEnv  = "SLCTL_DIRECT_POD_IPS"
	directDialTimout = 300 * time.Millisecond
)

// parseDirectMode resolves the direct-pod-IP mode from the --direct flag and
// the SLCTL_DIRECT_POD_IPS env var. The flag wins; unrecognized env values
// fall back to auto.
func parseDirectMode(forceFlag bool, env string) string {
	if forceFlag {
		return directAlways
	}
	switch env {
	case directAlways, "1", "true":
		return directAlways
	case directNever, "0", "false":
		return directNever
	default:
		return directAuto
	}
}

// fibRoute is one entry of `ip -j route show` output (iproute2 JSON). Only the
// fields we evaluate are declared; everything else is ignored.
type fibRoute struct {
	Dst  string `json:"dst"`
	Type string `json:"type"`
}

// routeCovers reports whether the routing table (as rendered by
// `ip -j route show`) holds a specific — i.e. non-default — route covering ip,
// and that the longest-prefix match among the specific routes is a route that
// actually forwards (not blackhole/unreachable/prohibit/throw). Returns the
// covering route's dst for --verbose output.
func routeCovers(routesJSON []byte, ip netip.Addr) (bool, string, error) {
	var routes []fibRoute
	if err := json.Unmarshal(routesJSON, &routes); err != nil {
		return false, "", fmt.Errorf("parse `ip -j route show` output: %w", err)
	}

	best := -1 // longest matching prefix length seen so far
	bestDst := ""
	bestForwards := false
	for _, r := range routes {
		if r.Dst == "default" || r.Dst == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(r.Dst)
		if err != nil {
			// Host route: `ip` prints a bare address with no /len.
			addr, aerr := netip.ParseAddr(r.Dst)
			if aerr != nil {
				continue // unparseable entry — ignore, stay conservative
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if !prefix.Contains(ip) || prefix.Bits() <= best {
			continue
		}
		best = prefix.Bits()
		bestDst = r.Dst
		switch r.Type {
		case "", "unicast", "local":
			bestForwards = true
		default: // blackhole, unreachable, prohibit, throw, …
			bestForwards = false
		}
	}
	if best < 0 || !bestForwards {
		return false, "", nil
	}
	return true, bestDst, nil
}

// directProber decides whether a pod IP is directly reachable from this host.
// listRoutes and dial are injectable for tests; production wiring is
// newDirectProber.
type directProber struct {
	mode       string
	listRoutes func(v6 bool) ([]byte, error)
	dial       func(hostport string) error
}

func newDirectProber(forceFlag bool) *directProber {
	return &directProber{
		mode: parseDirectMode(forceFlag, os.Getenv(directPodIPsEnv)),
		listRoutes: func(v6 bool) ([]byte, error) {
			family := "-4"
			if v6 {
				family = "-6"
			}
			out, err := exec.Command("ip", "-j", family, "route", "show").Output()
			if err != nil {
				return nil, fmt.Errorf("ip %s route show: %w", family, err)
			}
			return out, nil
		},
		dial: func(hostport string) error {
			c, err := net.DialTimeout("tcp", hostport, directDialTimout)
			if err != nil {
				return err
			}
			return c.Close()
		},
	}
}

// probe decides whether to connect to podIP:port directly, returning the
// decision and a human-readable reason for --verbose output.
func (p *directProber) probe(podIP string, port int) (bool, string) {
	if p.mode == directNever {
		return false, "direct pod IPs disabled (" + directPodIPsEnv + "=never)"
	}
	addr, err := netip.ParseAddr(podIP)
	if err != nil {
		return false, "no pod IP known for the target pod"
	}
	hostport := net.JoinHostPort(podIP, fmt.Sprintf("%d", port))

	via := "forced by --direct/" + directPodIPsEnv + "=always"
	if p.mode == directAuto {
		routes, err := p.listRoutes(addr.Is6())
		if err != nil {
			return false, fmt.Sprintf("route table not readable (%v)", err)
		}
		covered, dst, err := routeCovers(routes, addr)
		if err != nil {
			return false, err.Error()
		}
		if !covered {
			return false, fmt.Sprintf("no specific route to %s on this host", podIP)
		}
		via = fmt.Sprintf("route %s covers %s", dst, podIP)
	}

	if err := p.dial(hostport); err != nil {
		return false, fmt.Sprintf("%s, but TCP probe to %s failed: %v", via, hostport, err)
	}
	return true, fmt.Sprintf("%s; TCP probe to %s ok", via, hostport)
}
