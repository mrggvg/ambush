package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// GatewayAPI exposes pool/session endpoints and a feedback receiver.
// All routes are protected by a static bearer token.
type GatewayAPI struct {
	pool     *Pool
	sessions *SessionStore
	router   *Router
	token    string // GATEWAY_ADMIN_TOKEN; empty → endpoints disabled
}

const maxSessionTTL = 24 * time.Hour

// Register mounts the API routes on mux. Call with http.DefaultServeMux in production.
// If token is empty a warning is logged and no routes are registered.
func (a *GatewayAPI) Register(mux *http.ServeMux) {
	if a.token == "" {
		slog.Warn("GATEWAY_ADMIN_TOKEN not set — /api/v1/* endpoints are disabled")
		return
	}
	mux.Handle("GET /api/v1/exits", a.auth(http.HandlerFunc(a.exits)))
	mux.Handle("GET /api/v1/stats", a.auth(http.HandlerFunc(a.stats)))
	mux.Handle("POST /api/v1/feedback", a.auth(http.HandlerFunc(a.feedback)))
	mux.Handle("POST /api/v1/sessions", a.auth(http.HandlerFunc(a.createSession)))
}

func (a *GatewayAPI) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+a.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type exitNodeView struct {
	ID           string `json:"id"`
	IP           string `json:"ip"`
	Country      string `json:"country,omitempty"`
	NodeType     string `json:"node_type,omitempty"`
	ActiveStreams int32  `json:"active_streams"`
	Health       string `json:"health"` // "healthy" | "degraded"
}

func (a *GatewayAPI) exits(w http.ResponseWriter, r *http.Request) {
	entries := a.pool.snapshot()
	views := make([]exitNodeView, 0, len(entries))
	for _, e := range entries {
		health := "healthy"
		if e.health.isDegraded() {
			health = "degraded"
		}
		views = append(views, exitNodeView{
			ID:           strconv.FormatUint(e.id, 10),
			IP:           e.ip,
			Country:      e.country,
			NodeType:     e.nodeType,
			ActiveStreams: e.activeStreams.Load(),
			Health:       health,
		})
	}
	jsonOK(w, views)
}

func (a *GatewayAPI) stats(w http.ResponseWriter, r *http.Request) {
	entries := a.pool.snapshot()
	var totalStreams int32
	for _, e := range entries {
		totalStreams += e.activeStreams.Load()
	}
	jsonOK(w, map[string]any{
		"exitnodes":      len(entries),
		"active_streams": totalStreams,
	})
}

func (a *GatewayAPI) feedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionToken string `json:"session_token"`
		Success      bool   `json:"success"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionToken == "" {
		http.Error(w, "bad request: session_token required", http.StatusBadRequest)
		return
	}
	se := a.sessions.Get(body.SessionToken)
	if se == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	se.health.record(body.Success)
	slog.Info("feedback recorded", "token", body.SessionToken, "success", body.Success, "exitnode_id", se.id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *GatewayAPI) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TTLSeconds int    `json:"ttl_seconds"`
		Country    string `json:"country"`
		NodeType   string `json:"node_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ttl := defaultSessionTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
		if ttl > maxSessionTTL {
			ttl = maxSessionTTL
		}
	}

	c := Constraints{Country: body.Country, NodeType: body.NodeType}
	token, expiresAt, se, err := a.router.CreateSession(c, ttl)
	if err != nil {
		slog.Warn("create session: no eligible exit nodes", "country", body.Country, "node_type", body.NodeType)
		http.Error(w, "no eligible exit nodes", http.StatusServiceUnavailable)
		return
	}

	type exitView struct {
		ID       string `json:"id"`
		IP       string `json:"ip"`
		Country  string `json:"country,omitempty"`
		NodeType string `json:"node_type,omitempty"`
	}
	jsonOK(w, map[string]any{
		"token":      token,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"exit": exitView{
			ID:       strconv.FormatUint(se.id, 10),
			IP:       se.ip,
			Country:  se.country,
			NodeType: se.nodeType,
		},
	})
}
