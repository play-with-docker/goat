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

var udpStreams map[string]net.Conn

func main() {
	boundedInterfaces = map[string]bool{}
	udpStreams = map[string]net.Conn{}

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
	go func() {
		c := time.Tick(time.Second)
		for range c {
			bind(ipnet, bindPorts)
		}
	}()
	go func() {
		c := time.Tick(time.Second)
		for range c {
			if tunnel != nil {
				_, err := tunnel.Ping()
				if err != nil {
					tunnel.Close()
					log.Printf("Client disconnected %s\n", tunnel.RemoteAddr().String())
					tunnel = nil
				}
			}
		}
	}()
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
					for _, port := range ports {
						if port.Protocol == "tcp" {
							l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", ip, port.Port))
							if err != nil {
								log.Println(err)
								return
							}
							go func() {
								for {
									c, err := l.Accept()
									if err != nil {
										log.Println(err)
										continue
									}
									go tunnelTCP("tcp", ip, port.Port, c)
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
								udpConn, err := net.ListenUDP("udp", serverAddr)
								if err != nil {
									log.Println(err)
									return
								}
								buf := make([]byte, 1600)
								for {
									n, src, err := udpConn.ReadFromUDP(buf)
									if err != nil {
										log.Println(err)
										continue
									}
									packet := buf[:n]
									go tunnelUDP(ip, port.Port, packet, src, udpConn)
								}
							}()
						}
					}
					boundedInterfaces[ip] = true
				}
			}
		}
	}
}

func tunnelUDP(ip string, port int, packet []byte, src *net.UDPAddr, udpConn *net.UDPConn) {
	if tunnel == nil {
		log.Printf("No tunnel has been made")
		return
	}

	if _, found := udpStreams[src.String()]; !found {
		stream, err := tunnel.Open()
		if err != nil {
			panic(err)
		}

		go func() {
			defer func() {
				stream.Close()
				delete(udpStreams, src.String())
			}()

			buf := make([]byte, 1600)
			for {
				n, err := stream.Read(buf)
				if err != nil {
					return
				}
				b := buf[:n]
				_, err = udpConn.WriteToUDP(b, src)
				if err != nil {
					return
				}
			}
		}()

		header := fmt.Sprintf("%s %s %d", "udp", ip, port)
		if len(header) < 25 {
			header = fmt.Sprintf("%s%s", header, strings.Repeat(" ", 25-len(header)))
		}
		header = fmt.Sprintf("%s\n", header)
		stream.Write([]byte(header))

		udpStreams[src.String()] = stream
	}

	stream := udpStreams[src.String()]
	_, err := stream.Write(packet)
	if err != nil {
		panic(err)
	}
}

func tunnelTCP(protocol, ip string, port int, c net.Conn) {
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

	header := fmt.Sprintf("%s %s %d", protocol, ip, port)
	if len(header) < 25 {
		header = fmt.Sprintf("%s%s", header, strings.Repeat(" ", 25-len(header)))
	}
	header = fmt.Sprintf("%s\n", header)
	stream.Write([]byte(header))

	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}

	go cp(c, stream)
	go cp(stream, c)

	<-errc

	return
}

func registerTunnel(c net.Conn) {
	if tunnel != nil {
		log.Printf("There is already an opened tunnel. Disconnecting client %s.\n", tunnel.RemoteAddr().String())
		tunnel.Close()
	}
	session, err := yamux.Client(c, nil)
	if err != nil {
		panic(err)
	}
	tunnel = session
	log.Printf("Client connected from: %s", c.RemoteAddr().String())
}
