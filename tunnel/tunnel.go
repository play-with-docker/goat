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

func main() {
	dialer = net.Dialer{Timeout: time.Second * 5}
	flag.Parse()

	laddr := flag.Arg(0)

	if laddr == "" {
		log.Fatal("Please specify host:port to connect to")
	}

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
		panic(err)
	}
	session = s

	log.Printf("Connected to %s\n", laddr)
	for {
		stream, err := session.Accept()
		if err != nil {
			log.Printf("Disconnected from %s\n", laddr)
			return
		}
		line := make([]byte, 26)
		n, err := stream.Read(line)
		if err != nil {
			log.Println(err)
			stream.Close()
			continue
		}
		if n != 26 {
			log.Printf("Session header is wrong. Got: [%s]\n", string(line))
			stream.Close()
			continue
		}
		l := strings.TrimSpace(string(line))
		chunks := strings.Split(l, " ")
		if len(chunks) != 3 {
			log.Printf("Session header is wrong. Got: [%s]\n", l)
			stream.Close()
			continue
		}
		port, err := strconv.Atoi(chunks[2])
		if err != nil {
			log.Printf("Session header is wrong. Got: [%s]\n", l)
			stream.Close()
			continue
		}
		go tunnel(chunks[0], chunks[1], port, stream)
	}
}

func tunnel(protocol, ip string, port int, c net.Conn) {
	defer c.Close()

	var conn net.Conn
	raddr := fmt.Sprintf("%s:%d", ip, port)
	if protocol == "tcp" {
		co, err := net.Dial("tcp", raddr)
		if err != nil {
			log.Println(err)
			return
		}
		conn = co
	} else {
		addr, _ := net.ResolveUDPAddr("udp", raddr)
		co, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			log.Println(err)
			return
		}
		conn = co
	}
	defer conn.Close()

	log.Printf("Tunneling [%s] to [%s:%d]\n", protocol, ip, port)

	go io.Copy(conn, c)
	io.Copy(c, conn)
	log.Println("Stopped tunneling")
}
