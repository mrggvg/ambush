package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"
	socks5 "github.com/things-go/go-socks5"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Pool holds active exitnode yamux sessions.
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

// dbCredentialStore validates SOCKS5 username/password against the database.
type dbCredentialStore struct {
	db *pgxpool.Pool
}

func (s *dbCredentialStore) Valid(user, password, _ string) bool {
	var ok bool
	err := s.db.QueryRow(context.Background(),
		`SELECT EXISTS(
			SELECT 1 FROM proxy_credentials
			WHERE username = $1
			  AND password_hash = crypt($2, password_hash)
			  AND is_active = true
		)`, user, password,
	).Scan(&ok)
	if err != nil {
		log.Printf("socks5 auth db error: %v", err)
		return false
	}
	return ok
}

func validateExitNodeToken(ctx context.Context, db *pgxpool.Pool, token string) bool {
	hash := sha256Hex(token)
	var ok bool
	err := db.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM exit_node_tokens
			WHERE token_hash = $1
			  AND is_active = true
			  AND (expires_at IS NULL OR expires_at > now())
		)`, hash,
	).Scan(&ok)
	if err != nil {
		log.Printf("token auth db error: %v", err)
		return false
	}
	return ok
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func main() {
	db, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		log.Fatalf("db ping failed: %v", err)
	}
	log.Println("connected to database")

	pool := &Pool{}

	http.HandleFunc("/exitnode", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if !validateExitNodeToken(r.Context(), db, token) {
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
			socks5.WithCredential(&dbCredentialStore{db: db}),
			socks5.WithDial(pool.Dial),
		)
		socks5Addr := os.Getenv("SOCKS5_ADDR")
		if socks5Addr == "" {
			socks5Addr = ":1080"
		}
		log.Printf("SOCKS5 listening on %s", socks5Addr)
		log.Fatal(proxy.ListenAndServe("tcp", socks5Addr))
	}()

	gatewayAddr := os.Getenv("GATEWAY_ADDR")
	if gatewayAddr == "" {
		gatewayAddr = ":8080"
	}
	log.Printf("gateway listening on %s", gatewayAddr)
	log.Fatal(http.ListenAndServe(gatewayAddr, nil))
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
