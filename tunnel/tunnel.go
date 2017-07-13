package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/yamux"
)

func main() {
	flag.Parse()

	laddr := flag.Arg(0)

	if laddr == "" {
		log.Fatal("Please specify host:port to connect to")
	}

	conn, err := net.Dial("tcp", laddr)
	if err != nil {
		log.Fatal(err)
	}

	session, err := yamux.Server(conn, nil)
	if err != nil {
		panic(err)
	}

	for {
		stream, err := session.Accept()
		if err != nil {
			panic(err)
		}
		lb, _, err := bufio.NewReader(stream).ReadLine()
		if err != nil {
			log.Println(err)
			stream.Close()
			continue
		}
		l := string(lb)
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
}
