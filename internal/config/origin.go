package config

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

func ValidOrigin(value string) bool {
	if len(value) > 512 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || value != u.Scheme+"://"+u.Host || strings.ContainsAny(u.Host, "*%") {
		return false
	}
	if strings.HasSuffix(u.Hostname(), ".") || strings.ContainsAny(u.Hostname(), " ,;\\") {
		return false
	}
	if port := u.Port(); port != "" {
		v, e := strconv.Atoi(port)
		if e != nil || v < 1 || v > 65535 {
			return false
		}
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}
