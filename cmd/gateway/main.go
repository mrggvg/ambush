package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/things-go/go-socks5"
)

const (
	exitNodeToken = "hardcoded-secret-token"
	socksUser     = "user"
	socksPass     = "password"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Pool struct {
	mu       sync.Mutex
	sessions []*yamux.Session
}

func (p *Pool) add(s *yamux.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions = append(p.sessions, s)
	log.Printf("pool: exitnode added (%d total)", len(p.sessions))
}

func (p *Pool) remove(s *yamux.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, sess := range p.sessions {
		if sess == s {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			break
		}
	}
	log.Printf("pool: exitnode removed (%d total)", len(p.sessions))
}

func (p *Pool) pick() *yamux.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	alive := make([]*yamux.Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		if !s.IsClosed() {
			alive = append(alive, s)
		}
	}
	if len(alive) == 0 {
		return nil
	}
	return alive[rand.IntN(len(alive))]
}

func (p *Pool) Dial(_ context.Context, _, addr string) (net.Conn, error) {
	session := p.pick()
	if session == nil {
		return nil, errors.New("no exitnodes available")
	}
	stream, err := session.Open()
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(stream, "%s\n", addr); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

func main() {
	pool := &Pool{}

	http.HandleFunc("/exitnode", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != exitNodeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade failed: %v", err)
			return
		}

		session, err := yamux.Server(&wsConn{conn: conn}, nil)
		if err != nil {
			log.Printf("yamux failed: %v", err)
			_ = conn.Close()
			return
		}

		log.Printf("exitnode connected from %s", r.RemoteAddr)
		pool.add(session)
		defer func() {
			pool.remove(session)
			_ = session.Close()
		}()

		<-session.CloseChan()
	})

	go func() {
		proxy := socks5.NewServer(
			socks5.WithCredential(socks5.StaticCredentials{socksUser: socksPass}),
			socks5.WithDial(pool.Dial),
		)
		log.Println("SOCKS5 listening on :1080")
		log.Fatal(proxy.ListenAndServe("tcp", ":1080"))
	}()

	log.Println("gateway listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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
