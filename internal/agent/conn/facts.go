package conn

import (
	"net"
	"os"
	"runtime"
	"strconv"
)

// AutoFacts derives stable host facts at agent start. They are merged with
// user-provided labels in Hello.facts. Anything that needs /proc parsing is
// done by the system_info collector — these facts are limited to cheap,
// always-available bits.
func AutoFacts() map[string]string {
	out := map[string]string{
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
		"cpu_count": strconv.Itoa(runtime.NumCPU()),
	}
	if h, err := os.Hostname(); err == nil {
		out["hostname"] = h
	}
	if ip := primaryIP(); ip != "" {
		out["primary_ip"] = ip
	}
	return out
}

func primaryIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return ""
}
