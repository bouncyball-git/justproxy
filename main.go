package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const dialTimeout = 10 * time.Second

type PortMapping struct {
	Listen int `json:"listen"`
	Dest   int `json:"dest,omitempty"`
}

type Config struct {
	AllowedIPs []string      `json:"allowed_ips"`
	DestIP     string        `json:"dest_ip"`
	Ports      []PortMapping `json:"ports"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\nTCP proxy that forwards connections from allowed IPs to a destination.\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
	}
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	data, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	if len(cfg.AllowedIPs) == 0 {
		log.Fatal("no allowed_ips configured")
	}
	if cfg.DestIP == "" {
		log.Fatal("no dest_ip configured")
	}
	if len(cfg.Ports) == 0 {
		log.Fatal("no ports configured")
	}

	allowed := parseAllowedIPs(cfg.AllowedIPs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Bind all listeners before accepting connections so that
	// port conflicts are caught early and reported synchronously.
	type listenerInfo struct {
		ln       net.Listener
		destPort int
	}
	listeners := make([]listenerInfo, 0, len(cfg.Ports))
	for _, pm := range cfg.Ports {
		destPort := pm.Dest
		if destPort == 0 {
			destPort = pm.Listen
		}
		addr := fmt.Sprintf(":%d", pm.Listen)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("listen %s: %v", addr, err)
		}
		destAddr := net.JoinHostPort(cfg.DestIP, fmt.Sprintf("%d", destPort))
		log.Printf("listening on %s -> %s", addr, destAddr)
		listeners = append(listeners, listenerInfo{ln: ln, destPort: destPort})
	}

	var wg sync.WaitGroup
	for _, li := range listeners {
		wg.Add(1)
		go func(ln net.Listener, destPort int) {
			defer wg.Done()
			serve(ctx, ln, cfg.DestIP, destPort, allowed)
		}(li.ln, li.destPort)
	}

	<-sigCh
	log.Println("shutting down...")
	cancel()
	for _, li := range listeners {
		li.ln.Close()
	}
	wg.Wait()
}

// parseAllowedIPs normalizes IP addresses so that IPv4-mapped IPv6
// addresses (e.g. ::ffff:192.168.1.100) match their IPv4 equivalents.
func parseAllowedIPs(raw []string) map[string]bool {
	allowed := make(map[string]bool, len(raw))
	for _, s := range raw {
		ip := net.ParseIP(s)
		if ip == nil {
			log.Fatalf("invalid IP in allowed_ips: %q", s)
		}
		// Unmap IPv4-mapped IPv6 to canonical IPv4 form.
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		allowed[ip.String()] = true
	}
	return allowed
}

// normalizeIP returns the canonical string form of an IP address,
// unmapping IPv4-mapped IPv6 addresses.
func normalizeIP(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func serve(ctx context.Context, ln net.Listener, destIP string, destPort int, allowed map[string]bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(conn, destIP, destPort, allowed)
	}
}

func handleConn(src net.Conn, destIP string, destPort int, allowed map[string]bool) {
	clientIP, _, err := net.SplitHostPort(src.RemoteAddr().String())
	if err != nil {
		src.Close()
		return
	}

	if !allowed[normalizeIP(clientIP)] {
		log.Printf("rejected connection from %s", clientIP)
		src.Close()
		return
	}

	destAddr := net.JoinHostPort(destIP, fmt.Sprintf("%d", destPort))
	dst, err := net.DialTimeout("tcp", destAddr, dialTimeout)
	if err != nil {
		log.Printf("dial %s: %v", destAddr, err)
		src.Close()
		return
	}

	log.Printf("proxying %s -> %s", src.RemoteAddr(), destAddr)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		io.Copy(src, dst)
		if tc, ok := src.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
	dst.Close()
	src.Close()
}
