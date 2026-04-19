package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

func main() {
	gatewayURL := os.Getenv("GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = "ws://localhost:8080"
	}
	token := os.Getenv("EXIT_NODE_TOKEN")
	if token == "" {
		log.Fatal("EXIT_NODE_TOKEN is required")
	}

	for {
		if err := connect(gatewayURL, token); err != nil {
			log.Printf("connection lost: %v — retrying in 5s", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func connect(gatewayURL, token string) error {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return err
	}
	u.Path = "/exitnode"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	log.Printf("connected to gateway at %s", u.Host)

	session, err := yamux.Client(&wsConn{conn: conn}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()

	log.Println("yamux session ready")

	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		go handleStream(stream)
	}
}

func handleStream(stream net.Conn) {
	defer func() { _ = stream.Close() }()

	br := bufio.NewReader(stream)
	addr, err := br.ReadString('\n')
	if err != nil {
		log.Printf("stream: failed to read addr: %v", err)
		return
	}
	addr = strings.TrimSpace(addr)

	target, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("stream: dial %s failed: %v", addr, err)
		return
	}
	defer func() { _ = target.Close() }()

	log.Printf("stream: relaying to %s", addr)

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stream, target); done <- struct{}{} }()
	<-done
}

// wsConn wraps a gorilla WebSocket connection as a net.Conn for yamux.
type wsConn struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	reader io.Reader
}

func (c *wsConn) Read(b []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if err == io.EOF {
				c.reader = nil
				continue
			}
			return n, err
		}
		_, r, err := c.conn.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}
}

func (c *wsConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) Close() error                       { return c.conn.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { return c.conn.SetWriteDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
