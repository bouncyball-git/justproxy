package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
)

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

	allowed := make(map[string]bool, len(cfg.AllowedIPs))
	for _, ip := range cfg.AllowedIPs {
		allowed[ip] = true
	}

	var wg sync.WaitGroup
	for _, pm := range cfg.Ports {
		destPort := pm.Dest
		if destPort == 0 {
			destPort = pm.Listen
		}
		wg.Add(1)
		go func(listenPort, destPort int) {
			defer wg.Done()
			serve(listenPort, cfg.DestIP, destPort, allowed)
		}(pm.Listen, destPort)
	}
	wg.Wait()
}

func serve(listenPort int, destIP string, destPort int, allowed map[string]bool) {
	addr := fmt.Sprintf(":%d", listenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Printf("listening on %s -> %s:%d", addr, destIP, destPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept on %s: %v", addr, err)
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

	if !allowed[clientIP] {
		log.Printf("rejected connection from %s", clientIP)
		src.Close()
		return
	}

	destAddr := fmt.Sprintf("%s:%d", destIP, destPort)
	dst, err := net.Dial("tcp", destAddr)
	if err != nil {
		log.Printf("dial %s: %v", destAddr, err)
		src.Close()
		return
	}

	log.Printf("proxying %s -> %s", src.RemoteAddr(), destAddr)

	go func() {
		io.Copy(dst, src)
		dst.Close()
	}()
	io.Copy(src, dst)
	src.Close()
}
