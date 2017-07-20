package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

type tcpTracker struct {
	listener net.Listener
}

type udpTracker struct {
	listener *net.UDPConn
	conns    map[string]net.Conn
}

type vxlanTracker struct {
	listener *net.UDPConn
	conn     net.Conn
	stream   net.Conn
}

type tunnel struct {
	tcp2377 *tcpTracker
	tcp7946 *tcpTracker
	udp7946 *udpTracker
	udp4789 *vxlanTracker
}

var boundedInterfaces map[string]*tunnel

var client *yamux.Session
var localIP string

func main() {
	boundedInterfaces = map[string]*tunnel{}

	flag.Parse()

	laddr := flag.Arg(0)

	if laddr == "" {
		log.Fatal("Please specify host:port to bind to")
	}

	localIP = strings.Split(laddr, ":")[0]

	netAddr := flag.Arg(1)
	if netAddr == "" {
		log.Fatal("Missing network address")
	}

	_, ipnet, err := net.ParseCIDR(netAddr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		c := time.Tick(time.Second)
		for range c {
			bind(ipnet)
		}
	}()
	go func() {
		c := time.Tick(time.Second)
		for range c {
			if client != nil {
				_, err := client.Ping()
				if err != nil {
					client.Close()
					log.Printf("Client disconnected %s\n", client.RemoteAddr().String())
					client = nil
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

func bind(ipnet *net.IPNet) {
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
					log.Printf("Found interface with IP [%s] which is contained in the given net [%s]. Binding ports.\n", ip, ipnet)
					tu, err := openTunnel(ip)
					if err != nil {
						log.Println(err)
						continue
					}
					boundedInterfaces[ip] = tu
				}
			}
		}
	}
}

func openTunnel(iface string) (*tunnel, error) {
	tunn := &tunnel{}

	t, err := openTcpTunnel(iface, 2377)
	if err != nil {
		return nil, err
	}
	tunn.tcp2377 = t

	t, err = openTcpTunnel(iface, 7946)
	if err != nil {
		return nil, err
	}
	tunn.tcp7946 = t

	u, err := openUdpTunnel(iface, 7946)
	if err != nil {
		return nil, err
	}
	tunn.udp7946 = u

	v, err := openVXLANTunnel(iface)
	if err != nil {
		return nil, err
	}
	tunn.udp4789 = v

	return tunn, nil
}

func openTcpTunnel(iface string, port int) (*tcpTracker, error) {
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", iface, port))
	if err != nil {
		return nil, err
	}

	tracker := &tcpTracker{listener: l}

	go func() {
		for {
			c, err := tracker.listener.Accept()
			if err != nil {
				log.Println(err)
				continue
			}

			go tunnelTCP(iface, port, c)
		}
	}()

	return tracker, nil
}

func sendStreamHeader(stream net.Conn, protocol, ip string, port int) {
	header := fmt.Sprintf("%s %s %d", protocol, ip, port)
	if len(header) < 25 {
		header = fmt.Sprintf("%s%s", header, strings.Repeat(" ", 25-len(header)))
	}
	header = fmt.Sprintf("%s\n", header)
	stream.Write([]byte(header))
}

func openVXLANTunnel(iface string) (*vxlanTracker, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:4789", iface))
	if err != nil {
		return nil, err
	}

	l, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		return nil, err
	}

	c, err := net.Dial("udp", fmt.Sprintf("%s:4789", localIP))
	if err != nil {
		return nil, err
	}

	tracker := &vxlanTracker{listener: l, conn: c}
	if client != nil {
		stream, err := client.Open()
		if err != nil {
			log.Println("Error opening stream to tunnel VXLAN.")
			return nil, err
		}
		tracker.stream = stream
		sendStreamHeader(stream, "udp", iface, 4789)
		go func() {
			defer func() {
				tracker.stream.Close()
				tracker.stream = nil
				tracker.conn.Close()
				tracker.conn = nil
			}()

			io.Copy(tracker.conn, tracker.stream)
		}()
	}

	go func() {
		buf := make([]byte, 1600)
		for {
			n, _, err := tracker.listener.ReadFromUDP(buf)
			if err != nil {
				log.Println(err)
				continue
			}
			if tracker.stream == nil && client != nil {
				stream, err := client.Open()
				if err != nil {
					log.Println("Error opening stream to tunnel VXLAN.")
					continue
				}
				tracker.stream = stream
				sendStreamHeader(stream, "udp", iface, 4789)
				go func() {
					defer func() {
						tracker.stream.Close()
						tracker.stream = nil
						tracker.conn.Close()
						tracker.conn = nil
					}()

					io.Copy(tracker.conn, tracker.stream)
				}()
			}
			go func(packet []byte) {
				_, err := tracker.stream.Write(packet)
				if err != nil {
					tracker.stream.Close()
					tracker.stream = nil
				}
			}(buf[:n])
		}
	}()

	return tracker, nil
}

func openUdpTunnel(iface string, port int) (*udpTracker, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", iface, port))
	if err != nil {
		return nil, err
	}

	l, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		return nil, err
	}

	tracker := &udpTracker{listener: l, conns: make(map[string]net.Conn)}

	go func() {
		buf := make([]byte, 1600)
		for {
			n, src, err := tracker.listener.ReadFromUDP(buf)
			if err != nil {
				log.Println(err)
				continue
			}
			packet := buf[:n]
			go tunnelUDP(iface, port, packet, src, tracker)
		}
	}()

	return tracker, nil
}

func tunnelUDP(ip string, port int, packet []byte, src *net.UDPAddr, tracker *udpTracker) {
	if client == nil {
		log.Printf("No client connected.")
		return
	}

	if _, found := tracker.conns[src.String()]; !found {
		var stream net.Conn
		var err error
		for i := 0; i < 10; i++ {
			if client == nil {
				break
			}
			stream, err = client.Open()
			if err != nil {
				log.Println("Error opening stream to tunnel UDP. Got: ", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			break
		}
		if stream == nil {
			log.Println("Could not open stream to tunnel UDP. Giving up.")
			return
		}

		go func(src *net.UDPAddr) {
			defer func() {
				stream.Close()
				delete(tracker.conns, src.String())
			}()

			buf := make([]byte, 1600)
			for {
				n, err := stream.Read(buf)
				if err != nil {
					return
				}
				b := buf[:n]
				_, err = tracker.listener.WriteToUDP(b, src)
				if err != nil {
					return
				}
			}
		}(src)

		header := fmt.Sprintf("%s %s %d", "udp", ip, port)
		if len(header) < 25 {
			header = fmt.Sprintf("%s%s", header, strings.Repeat(" ", 25-len(header)))
		}
		header = fmt.Sprintf("%s\n", header)
		stream.Write([]byte(header))

		tracker.conns[src.String()] = stream
	}

	stream := tracker.conns[src.String()]
	_, err := stream.Write(packet)
	if err != nil {
		log.Println("Error while writing to stream a UDP packet. Got: ", err)
		stream.Close()
		delete(tracker.conns, src.String())
	}
}

func tunnelTCP(ip string, port int, c net.Conn) {
	defer c.Close()

	if client == nil {
		log.Printf("No client connected.")
		return
	}

	var stream net.Conn
	var err error
	for i := 0; i < 10; i++ {
		if client == nil {
			break
		}
		stream, err = client.Open()
		if err != nil {
			log.Println("Error opening stream to tunnel TCP. Got: ", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	if stream == nil {
		log.Println("Could not open stream to tunnel TCP. Giving up.")
		return
	}
	defer stream.Close()

	header := fmt.Sprintf("%s %s %d", "tcp", ip, port)
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
	if client != nil {
		log.Printf("There is already a client connected. Disconnecting %s.\n", client.RemoteAddr().String())
		client.Close()
	}
	session, err := yamux.Client(c, nil)
	if err != nil {
		log.Println("Could not create yamux session. Got: ", err)
		return
	}
	client = session
	log.Printf("Client connected from: %s", c.RemoteAddr().String())
}
