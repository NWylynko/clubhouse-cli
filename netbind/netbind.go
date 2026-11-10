package netbind

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Set by cloudflare/box/src/provision.js: a Cloudflare Container has no
// address a peer can dial.
const AdvertiseNone = "none"

func DirectSuppressed() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CLUB_ADVERTISE")), AdvertiseNone)
}

func Advertise() string {
	adv := strings.TrimSpace(os.Getenv("CLUB_ADVERTISE"))
	if strings.EqualFold(adv, AdvertiseNone) {
		return ""
	}
	return adv
}

func ListenAdvertise(mode string, port int) (net.Listener, string, error) {
	adv := Advertise()
	if adv == "" {
		if port == 0 {
			return ListenMode(mode)
		}
		ln, host, err := ListenMode(mode)
		if err != nil || ln == nil {
			return ln, host, err
		}
		bindIP := ln.Addr().(*net.TCPAddr).IP.String()
		_ = ln.Close()
		fixed, err := net.Listen("tcp", net.JoinHostPort(bindIP, strconv.Itoa(port)))
		return fixed, host, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, "", fmt.Errorf("CLUB_ADVERTISE=%s: binding 0.0.0.0:%d: %w", adv, port, err)
	}
	return ln, adv, nil
}

func ListenMode(mode string) (net.Listener, string, error) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "auto":
		ip, err := RoutableIP()
		if err != nil {
			return nil, "", err
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		return ln, ip, err
	case "", "loopback", "local", "127.0.0.1":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		return ln, "127.0.0.1", err
	default:
		return nil, "", fmt.Errorf("bind mode %q is not 'auto' or loopback", m)
	}
}

func AdvertiseHostMode(mode string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "auto":
		return RoutableIP()
	case "", "loopback", "local", "127.0.0.1":
		return "127.0.0.1", nil
	default:
		return "", fmt.Errorf("bind mode %q is not 'auto' or loopback", m)
	}
}

func BindForControl(controlBase string) string {
	if strings.TrimSpace(controlBase) == "" || LoopbackURL(controlBase) {
		return ""
	}
	if _, err := RoutableIP(); err != nil {
		return ""
	}
	return "auto"
}

func RoutableIP() (string, error) {
	if ip, err := TailscaleIP(); err == nil {
		return ip, nil
	}
	ip, err := LANIP()
	if err != nil {
		return "", fmt.Errorf("no routable address (no Tailscale interface and %w)", err)
	}
	return ip, nil
}

func LoopbackURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "" {
		h = strings.Trim(raw, "[]")
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

func TailscaleIP() (string, error) {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil && cgnat.Contains(v4) {
			return v4.String(), nil
		}
	}
	return "", fmt.Errorf("no Tailscale address (100.64.0.0/10) on any interface — is tailscale up?")
}

func LANIP() (string, error) {
	c, err := net.Dial("udp", "203.0.113.1:9")
	if err == nil {
		defer c.Close()
		if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsLoopback() {
			return ua.IP.String(), nil
		}
	}
	addrs, aerr := net.InterfaceAddrs()
	if aerr != nil {
		return "", aerr
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 interface")
}
