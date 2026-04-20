package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"
	socks5 "github.com/things-go/go-socks5"
)

const (
	pingInterval = 30 * time.Second
	pongTimeout  = 10 * time.Second
	dbTimeout    = 5 * time.Second
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type dbCredentialStore struct {
	db *pgxpool.Pool
}

func (s *dbCredentialStore) Valid(user, password, _ string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	var ok bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM proxy_credentials
			WHERE username = $1
			  AND password_hash = crypt($2, password_hash)
			  AND is_active = true
		)`, user, password,
	).Scan(&ok)
	if err != nil {
		slog.Error("socks5 auth failed", "username", user, "error", err)
		return false
	}
	return ok
}

func validateExitNodeToken(ctx context.Context, db *pgxpool.Pool, token string) string {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
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
		slog.Error("token auth failed", "error", err)
		return ""
	}
	return id
}

func trackExitNodeIP(ctx context.Context, db *pgxpool.Pool, tokenID, remoteAddr string) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		slog.Error("ip tracking: parse addr failed", "addr", remoteAddr, "error", err)
		return
	}
	_, err = db.Exec(ctx,
		`INSERT INTO exit_node_ips (token_id, ip)
		 VALUES ($1, $2)
		 ON CONFLICT (token_id, ip) DO UPDATE SET last_seen_at = now()`,
		tokenID, ip,
	)
	if err != nil {
		slog.Error("ip tracking: upsert failed", "token_id", tokenID, "ip", ip, "error", err)
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	db, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		slog.Error("db connect failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		slog.Error("db ping failed", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to database")

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	pool := &Pool{metrics: metrics}

	maxStreams := int32(20)
	if v := os.Getenv("MAX_STREAMS_PER_CREDENTIAL"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			maxStreams = int32(n)
		}
	}
	slog.Info("credential rate limit configured", "max_streams_per_credential", maxStreams)
	router := NewRouter(pool, NewCredentialLimiter(maxStreams), metrics)

	http.Handle("/metrics", MetricsHandler(reg))

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
			slog.Error("websocket upgrade failed", "remote_addr", r.RemoteAddr, "error", err)
			return
		}

		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
		})
		_ = conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))

		wsc := &wsConn{conn: conn}
		session, err := yamux.Server(wsc, nil)
		if err != nil {
			slog.Error("yamux session failed", "remote_addr", r.RemoteAddr, "error", err)
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

		exitIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		entry := newSessionEntry(session, exitIP)
		slog.Info("exitnode connected", "exitnode_id", entry.id, "ip", exitIP, "token_id", tokenID)
		pool.add(entry)
		if peers := pool.byIP(exitIP); len(peers) > 1 {
			slog.Warn("multiple exit nodes share the same public IP — they will share cooldown slots per domain",
				"ip", exitIP, "count", len(peers))
		}
		defer func() {
			slog.Info("exitnode disconnected", "exitnode_id", entry.id, "ip", exitIP)
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
		slog.Error("socks5 listen failed", "error", err)
		os.Exit(1)
	}

	proxy := socks5.NewServer(
		socks5.WithCredential(&dbCredentialStore{db: db}),
		socks5.WithDialAndRequest(func(ctx context.Context, network, addr string, req *socks5.Request) (net.Conn, error) {
			username := ""
			if req.AuthContext != nil {
				username = req.AuthContext.Payload["Username"]
			}
			return router.DialWithUser(ctx, network, addr, username)
		}),
	)
	go func() {
		slog.Info("SOCKS5 listening", "addr", socks5Addr)
		if err := proxy.Serve(socks5Ln); err != nil {
			slog.Error("SOCKS5 stopped", "error", err)
		}
	}()

	gatewayAddr := os.Getenv("GATEWAY_ADDR")
	if gatewayAddr == "" {
		gatewayAddr = ":8080"
	}
	httpServer := &http.Server{Addr: gatewayAddr}

	go func() {
		certFile := os.Getenv("TLS_CERT")
		keyFile := os.Getenv("TLS_KEY")
		tls := certFile != "" && keyFile != ""
		slog.Info("gateway listening", "addr", gatewayAddr, "tls", tls)
		var err error
		if tls {
			err = httpServer.ListenAndServeTLS(certFile, keyFile)
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("gateway stopped", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down — draining active streams")
	_ = socks5Ln.Close()
	pool.waitStreams(30 * time.Second)

	slog.Info("closing exitnode sessions")
	pool.closeAll()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)

	slog.Info("shutdown complete")
}
