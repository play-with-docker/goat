package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

type bindPort struct {
	Protocol string
	Port     int
}

var boundedInterfaces map[string]bool

var tunnel *yamux.Session

func main() {
	boundedInterfaces = map[string]bool{}

	flag.Parse()

	laddr := flag.Arg(0)

	if laddr == "" {
		log.Fatal("Please specify host:port to bind to")
	}

	netAddr := flag.Arg(1)
	if netAddr == "" {
		log.Fatal("Missing network address")
	}

	ports := flag.Arg(2)
	if ports == "" {
		log.Fatal("Missing ports to bind to")
	}

	pts := strings.Split(ports, ",")
	bindPorts := make([]bindPort, len(pts))
	for i, p := range pts {
		chunks := strings.Split(p, ":")
		bindPorts[i].Protocol = chunks[0]
		port, err := strconv.Atoi(chunks[1])
		if err != nil {
			log.Fatal(err)
		}
		bindPorts[i].Port = port
	}

	_, ipnet, err := net.ParseCIDR(netAddr)
	if err != nil {
		log.Fatal(err)
	}
	time.AfterFunc(time.Second, func() {
		bind(ipnet, bindPorts)
	})
	l, err := net.Listen("tcp", laddr)
	if err != nil {
		log.Fatal(err)
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}

		go registerTunnel(conn)
	}
}

func bind(ipnet *net.IPNet, ports []bindPort) {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Println(err)
		return
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			log.Println(err)
			return
		}
		for _, addr := range addrs {
			ifaceIP, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				log.Println(err)
				return
			}

			ip := ifaceIP.String()

			if ipnet.Contains(ifaceIP) {
				if _, found := boundedInterfaces[ip]; !found {
					log.Printf("Found interface with IP [%s] which is contained in the given net [%s]. Binding ports: %v\n", ip, ipnet, ports)
					listeners := make([]net.Listener, len(ports))
					for i, port := range ports {
						if port.Protocol == "tcp" {
							l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", ip, port.Port))
							if err != nil {
								log.Println(err)
								return
							}
							listeners[i] = l
							go func() {
								for {
									c, err := l.Accept()
									if err != nil {
										log.Println(err)
										continue
									}
									go startTunnel("tcp", ip, port.Port, c)
								}
							}()
						} else {
							// it is UDP
							serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port.Port))
							if err != nil {
								log.Println(err)
								continue
							}
							go func() {
								serverConn, err := net.ListenUDP("udp", serverAddr)
								if err != nil {
									log.Println(err)
									return
								}
								startTunnel("udp", ip, port.Port, serverConn)
							}()
						}
					}
					boundedInterfaces[ip] = true
				}
			}
		}
	}
}

func startTunnel(protocol, ip string, port int, c net.Conn) {
	defer c.Close()

	if tunnel == nil {
		log.Printf("No tunnel has been made")
		return
	}

	stream, err := tunnel.Open()
	if err != nil {
		panic(err)
	}
	defer stream.Close()

	stream.Write([]byte(fmt.Sprintf("%s %s %d\n", protocol, ip, port)))
	go io.Copy(c, stream)
	io.Copy(stream, c)
}

func registerTunnel(c net.Conn) {
	if tunnel != nil {
		log.Printf("There is already an opened tunnel. Discarding.\n")
		c.Close()
		return
	}
	session, err := yamux.Client(c, nil)
	if err != nil {
		panic(err)
	}
	tunnel = session
}
