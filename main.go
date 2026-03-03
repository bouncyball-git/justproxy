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
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	dialTimeout = 10 * time.Second
	udpBufSize  = 65535
)

type PortMapping struct {
	Proto  string `json:"proto"`
	Listen int    `json:"listen"`
	Dest   string `json:"dest"`
}

type Config struct {
	AllowedIPs     []string      `json:"allowed_ips"`
	UDPIdleTimeout int           `json:"udp_idle_timeout"`
	Ports          []PortMapping `json:"ports"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\nTCP/UDP proxy that forwards connections from allowed IPs to a destination.\n\nOptions:\n", os.Args[0])
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
	if len(cfg.Ports) == 0 {
		log.Fatal("no ports configured")
	}

	allowed := parseAllowedIPs(cfg.AllowedIPs)

	idleTimeout := 30 * time.Second
	if cfg.UDPIdleTimeout > 0 {
		idleTimeout = time.Duration(cfg.UDPIdleTimeout) * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Closers collects everything that needs closing on shutdown.
	var closers []io.Closer
	var wg sync.WaitGroup

	for _, pm := range cfg.Ports {
		proto := strings.ToLower(pm.Proto)
		if proto == "" {
			proto = "tcp"
		}
		destAddr := resolveDestAddr(pm.Dest, pm.Listen)
		listenAddr := fmt.Sprintf(":%d", pm.Listen)

		switch proto {
		case "tcp":
			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				log.Fatalf("listen tcp %s: %v", listenAddr, err)
			}
			log.Printf("listening tcp on %s -> %s", listenAddr, destAddr)
			closers = append(closers, ln)
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveTCP(ctx, ln, destAddr, allowed)
			}()

		case "udp":
			pc, err := net.ListenPacket("udp", listenAddr)
			if err != nil {
				log.Fatalf("listen udp %s: %v", listenAddr, err)
			}
			log.Printf("listening udp on %s -> %s", listenAddr, destAddr)
			closers = append(closers, pc)
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveUDP(ctx, pc, destAddr, allowed, idleTimeout)
			}()

		default:
			log.Fatalf("unsupported proto %q for port %d", pm.Proto, pm.Listen)
		}
	}

	<-sigCh
	log.Println("shutting down...")
	cancel()
	for _, c := range closers {
		c.Close()
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
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		allowed[ip.String()] = true
	}
	return allowed
}

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

func resolveDestAddr(dest string, listenPort int) string {
	host, port, err := net.SplitHostPort(dest)
	if err != nil {
		if net.ParseIP(dest) == nil {
			log.Fatalf("invalid dest address: %q", dest)
		}
		return net.JoinHostPort(dest, fmt.Sprintf("%d", listenPort))
	}
	if net.ParseIP(host) == nil {
		log.Fatalf("invalid dest IP: %q", host)
	}
	return net.JoinHostPort(host, port)
}

// --- TCP ---

func serveTCP(ctx context.Context, ln net.Listener, destAddr string, allowed map[string]bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("accept: %v", err)
			continue
		}
		go handleTCPConn(conn, destAddr, allowed)
	}
}

func handleTCPConn(src net.Conn, destAddr string, allowed map[string]bool) {
	clientIP, _, err := net.SplitHostPort(src.RemoteAddr().String())
	if err != nil {
		src.Close()
		return
	}

	if !allowed[normalizeIP(clientIP)] {
		log.Printf("tcp: rejected %s", clientIP)
		src.Close()
		return
	}

	dst, err := net.DialTimeout("tcp", destAddr, dialTimeout)
	if err != nil {
		log.Printf("tcp: dial %s: %v", destAddr, err)
		src.Close()
		return
	}

	log.Printf("tcp: proxying %s -> %s", src.RemoteAddr(), destAddr)

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

// --- UDP ---

func serveUDP(ctx context.Context, pc net.PacketConn, destAddr string, allowed map[string]bool, idleTimeout time.Duration) {
	dest, err := net.ResolveUDPAddr("udp", destAddr)
	if err != nil {
		log.Printf("udp: resolve %s: %v", destAddr, err)
		return
	}

	// Track NAT mappings: client addr -> upstream conn.
	var mu sync.Mutex
	clients := make(map[string]*udpSession)

	buf := make([]byte, udpBufSize)
	for {
		n, clientAddr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("udp: read: %v", err)
			continue
		}

		clientIP, _, err := net.SplitHostPort(clientAddr.String())
		if err != nil {
			continue
		}
		if !allowed[normalizeIP(clientIP)] {
			log.Printf("udp: rejected %s", clientIP)
			continue
		}

		key := clientAddr.String()
		mu.Lock()
		sess, ok := clients[key]
		if !ok {
			upstream, err := net.DialUDP("udp", nil, dest)
			if err != nil {
				mu.Unlock()
				log.Printf("udp: dial %s: %v", destAddr, err)
				continue
			}
			log.Printf("udp: proxying %s -> %s", clientAddr, destAddr)
			sess = &udpSession{upstream: upstream}
			clients[key] = sess

			// Return path: upstream -> client.
			go func(cAddr net.Addr) {
				rbuf := make([]byte, udpBufSize)
				for {
					sess.upstream.SetReadDeadline(time.Now().Add(idleTimeout))
					rn, _, rerr := sess.upstream.ReadFromUDP(rbuf)
					if rerr != nil {
						break
					}
					pc.WriteTo(rbuf[:rn], cAddr)
				}
				mu.Lock()
				delete(clients, key)
				mu.Unlock()
				sess.upstream.Close()
			}(clientAddr)
		}
		mu.Unlock()

		sess.upstream.SetWriteDeadline(time.Now().Add(idleTimeout))
		sess.upstream.Write(buf[:n])
	}
}

type udpSession struct {
	upstream *net.UDPConn
}
