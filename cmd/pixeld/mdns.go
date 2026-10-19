package main

import (
	"context"
	"log"
	"net"
	"strconv"

	"github.com/grandcat/zeroconf"

	"pixeld/sendspin"
)

// advertise announces the ESPHome API over mDNS so Home Assistant discovers the light.
func advertise(ctx context.Context, name, addr, mac string) {
	_, portStr, _ := net.SplitHostPort(addr)
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return
	}
	var ifaces []net.Interface
	if ifc, err := net.InterfaceByName("wlan0"); err == nil {
		ifaces = append(ifaces, *ifc)
	}
	txt := []string{"version=2025.9.0", "mac=" + stripColons(mac), "board=h8630", "platform=HOST", "network=wifi",
		"friendly_name=Gaming Pixel Light"}
	srv, err := zeroconf.Register(name, "_esphomelib._tcp", "local.", port, txt, ifaces)
	if err != nil {
		log.Printf("mdns: %v", err)
		return
	}
	log.Printf("mdns: advertising %s._esphomelib._tcp on port %d", name, port)
	<-ctx.Done()
	srv.Shutdown()
}

// advertiseSendspin announces the Sendspin player so Music Assistant finds the light and connects to it.
func advertiseSendspin(ctx context.Context, name, addr string) {
	_, portStr, _ := net.SplitHostPort(addr)
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return
	}
	var ifaces []net.Interface
	if ifc, err := net.InterfaceByName("wlan0"); err == nil {
		ifaces = append(ifaces, *ifc)
	}
	srv, err := zeroconf.Register(name, "_sendspin._tcp", "local.", port, []string{"path=" + sendspin.Path, "name=" + name}, ifaces)
	if err != nil {
		log.Printf("mdns: %v", err)
		return
	}
	log.Printf("mdns: advertising %s._sendspin._tcp on port %d", name, port)
	<-ctx.Done()
	srv.Shutdown()
}

func stripColons(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
