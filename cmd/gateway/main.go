package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"
	socks5 "github.com/things-go/go-socks5"
)

const (
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

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

func validateExitNodeToken(ctx context.Context, db *pgxpool.Pool, token string) string {
	hash := sha256Hex(token)
	var id string
	err := db.QueryRow(ctx,
		`SELECT id FROM exit_node_tokens
		 WHERE token_hash = $1
		   AND is_active = true
		   AND (expires_at IS NULL OR expires_at > now())`,
		hash,
	).Scan(&id)
	if err != nil {
		log.Printf("token auth db error: %v", err)
		return ""
	}
	return id
}

func trackExitNodeIP(ctx context.Context, db *pgxpool.Pool, tokenID, remoteAddr string) {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		log.Printf("ip tracking: failed to parse addr %s: %v", remoteAddr, err)
		return
	}
	_, err = db.Exec(ctx,
		`INSERT INTO exit_node_ips (token_id, ip)
		 VALUES ($1, $2)
		 ON CONFLICT (token_id, ip) DO UPDATE SET last_seen_at = now()`,
		tokenID, ip,
	)
	if err != nil {
		log.Printf("ip tracking: upsert failed: %v", err)
	}
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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
	router := NewRouter(pool)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		entries := pool.snapshot()
		totalStreams := int32(0)
		for _, e := range entries {
			totalStreams += e.activeStreams.Load()
		}
		jsonOK(w, map[string]any{
			"status":        "ok",
			"exitnodes":     len(entries),
			"active_streams": totalStreams,
		})
	})

	http.HandleFunc("/exitnode", func(w http.ResponseWriter, r *http.Request) {
		tokenID := validateExitNodeToken(r.Context(), db, r.URL.Query().Get("token"))
		if tokenID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		trackExitNodeIP(r.Context(), db, tokenID, r.RemoteAddr)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade failed: %v", err)
			return
		}

		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
		})
		_ = conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))

		wsc := &wsConn{conn: conn}
		session, err := yamux.Server(wsc, nil)
		if err != nil {
			log.Printf("yamux failed: %v", err)
			_ = conn.Close()
			return
		}

		go func() {
			ticker := time.NewTicker(pingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := wsc.ping(pongTimeout); err != nil {
						_ = session.Close()
						return
					}
				case <-session.CloseChan():
					return
				}
			}
		}()

		entry := newSessionEntry(session)
		log.Printf("exitnode connected from %s", r.RemoteAddr)
		pool.add(entry)
		defer func() {
			pool.remove(entry)
			_ = session.Close()
		}()

		<-session.CloseChan()
	})

	socks5Addr := os.Getenv("SOCKS5_ADDR")
	if socks5Addr == "" {
		socks5Addr = ":1080"
	}
	socks5Ln, err := net.Listen("tcp", socks5Addr)
	if err != nil {
		log.Fatalf("socks5 listen failed: %v", err)
	}

	proxy := socks5.NewServer(
		socks5.WithCredential(&dbCredentialStore{db: db}),
		socks5.WithDial(router.Dial),
	)
	go func() {
		log.Printf("SOCKS5 listening on %s", socks5Addr)
		if err := proxy.Serve(socks5Ln); err != nil {
			log.Printf("SOCKS5 stopped: %v", err)
		}
	}()

	gatewayAddr := os.Getenv("GATEWAY_ADDR")
	if gatewayAddr == "" {
		gatewayAddr = ":8080"
	}
	httpServer := &http.Server{Addr: gatewayAddr}

	go func() {
		log.Printf("gateway listening on %s", gatewayAddr)
		log.Printf("health endpoint: http://%s/health", gatewayAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("gateway stopped: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("shutting down — draining active streams...")
	_ = socks5Ln.Close()
	pool.waitStreams(30 * time.Second)

	log.Println("closing exitnode sessions...")
	pool.closeAll()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)

	log.Println("shutdown complete")
}
