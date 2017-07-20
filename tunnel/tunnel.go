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

var session *yamux.Session
var dialer net.Dialer
var fdb map[string]net.Conn
var laddr string

func main() {
	fdb = make(map[string]net.Conn)
	dialer = net.Dialer{Timeout: time.Second * 5}
	flag.Parse()

	laddr = flag.Arg(0)

	if laddr == "" {
		log.Fatal("Please specify host:port to connect to")
	}

	go tunnelVXLAN()
	go func() {
		c := time.Tick(time.Second)
		for range c {
			if session != nil {
				_, err := session.Ping()
				if err != nil {
					log.Printf("Disconnected from %s. Reconnecting...\n", session.RemoteAddr().String())
					session.Close()
					session = nil
				}
			}
		}
	}()
	for {
		connect(laddr)
		time.Sleep(time.Second)
	}
}

func connect(laddr string) {
	log.Printf("Connecting to [%s]\n", laddr)
	conn, err := dialer.Dial("tcp", laddr)
	if err != nil {
		log.Printf("Could not connect to %s. Retrying...\n", laddr)
		return
	}

	s, err := yamux.Server(conn, nil)
	if err != nil {
		log.Println("Could not create yamux session. Got:", err)
		return
	}
	session = s

	log.Printf("Connected to %s\n", laddr)
	for {
		stream, err := session.Accept()
		if err != nil {
			log.Printf("Disconnected from %s\n", laddr)
			return
		}
		go tunnelStream(stream)
	}
}

func tunnelVXLAN() {
	serverAddr, err := net.ResolveUDPAddr("udp", ":4789")
	if err != nil {
		log.Fatal(err)
	}
	udpConn, err := net.ListenUDP("udp", serverAddr)
	log.Println("Listening for incoming VXLAN packets")
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 1600)
	for {
		n, _, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			log.Println(err)
			continue
		}
		go func(b []byte) {
			if c, found := fdb[laddr]; found {
				log.Printf("Received VXLAN packet. Tunneling.\n")
				_, err := c.Write(b)
				if err != nil {
					c.Close()
					delete(fdb, laddr)
				}
			}
		}(buf[:n])
	}
}

func tunnelStream(stream net.Conn) {
	line := make([]byte, 26)
	n, err := stream.Read(line)
	if err != nil {
		log.Println(err)
		stream.Close()
		return
	}
	if n != 26 {
		log.Printf("Session header is wrong. Got: [%s]\n", string(line))
		stream.Close()
		return
	}
	l := strings.TrimSpace(string(line))
	chunks := strings.Split(l, " ")
	if len(chunks) != 3 {
		log.Printf("Session header is wrong. Got: [%s]\n", l)
		stream.Close()
		return
	}
	port, err := strconv.Atoi(chunks[2])
	if err != nil {
		log.Printf("Session header is wrong. Got: [%s]\n", l)
		stream.Close()
		return
	}

	if chunks[0] == "UDP" && port == 4789 {
		// Need to tunnel vxlan
		// Learn connection details

		if _, found := fdb[laddr]; !found {
			fdb[laddr] = stream
		}
	}

	tunnel(chunks[0], chunks[1], port, stream)
}

func tunnel(protocol, ip string, port int, c net.Conn) {
	defer c.Close()

	raddr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := dialer.Dial(protocol, raddr)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	log.Printf("Tunneling [%s] to [%s:%d]\n", protocol, ip, port)

	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}

	go cp(conn, c)
	go cp(c, conn)

	<-errc

	log.Printf("Stopped tunneling [%s] to [%s:%d]\n", protocol, ip, port)
	return
}
