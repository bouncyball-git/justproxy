# justproxy
TCP/UDP reverse proxy that forwards connections from allowed IPs to a destination.

Usage: ./justproxy [options]

Options: -config "path to config file" (default "config.json")

Config example:
```
{
  "allowed_ips": ["127.0.0.1", "192.168.1.100"],
  "udp_idle_timeout": 30,
  "ports": [
    {"proto": "tcp", "listen": 8080, "dest": "10.0.0.1"},
    {"proto": "tcp", "listen": 3000, "dest": "10.0.0.100:3001"},
    {"proto": "udp", "listen": 5353, "dest": "10.0.0.1:53"}
  ]
}
```
