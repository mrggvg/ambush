package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	db *pgxpool.Pool
}

func main() {
	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		log.Fatal("ADMIN_TOKEN is required")
	}

	db, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		log.Fatalf("db ping failed: %v", err)
	}
	log.Println("connected to database")

	s := &Server{db: db}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(bearerAuth(adminToken))

	r.Post("/users", s.createUser)
	r.Get("/users", s.listUsers)

	r.Post("/users/{userID}/tokens", s.createToken)
	r.Get("/users/{userID}/tokens", s.listTokens)
	r.Delete("/tokens/{tokenID}", s.revokeToken)

	r.Post("/proxy/credentials", s.createProxyCredential)
	r.Get("/proxy/credentials", s.listProxyCredentials)
	r.Delete("/proxy/credentials/{credID}", s.revokeProxyCredential)

	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	log.Printf("api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}

func bearerAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// --- users ---

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		http.Error(w, "display_name required", http.StatusBadRequest)
		return
	}

	var id string
	err := s.db.QueryRow(r.Context(),
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`,
		body.DisplayName,
	).Scan(&id)
	if err != nil {
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		log.Printf("createUser: %v", err)
		return
	}

	jsonOK(w, map[string]any{"id": id, "display_name": body.DisplayName})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(),
		`SELECT id, display_name, created_at, is_active FROM users ORDER BY created_at DESC`,
	)
	if err != nil {
		http.Error(w, "failed to list users", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var users []map[string]any
	for rows.Next() {
		var id, displayName string
		var createdAt, isActive any
		if err := rows.Scan(&id, &displayName, &createdAt, &isActive); err != nil {
			continue
		}
		users = append(users, map[string]any{
			"id": id, "display_name": displayName,
			"created_at": createdAt, "is_active": isActive,
		})
	}
	jsonOK(w, users)
}

// --- exit node tokens ---

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")

	var body struct {
		Label     string  `json:"label"`
		ExpiresAt *string `json:"expires_at"` // RFC3339 or null
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Label == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}

	raw, err := generateToken()
	if err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	hash := sha256Hex(raw)

	var id string
	err = s.db.QueryRow(r.Context(),
		`INSERT INTO exit_node_tokens (user_id, token_hash, label, expires_at)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, hash, body.Label, body.ExpiresAt,
	).Scan(&id)
	if err != nil {
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		log.Printf("createToken: %v", err)
		return
	}

	// raw token returned once — never stored plain
	jsonOK(w, map[string]any{"id": id, "token": raw, "label": body.Label})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")

	rows, err := s.db.Query(r.Context(),
		`SELECT id, label, created_at, expires_at, is_active
		 FROM exit_node_tokens WHERE user_id = $1 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		http.Error(w, "failed to list tokens", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var tokens []map[string]any
	for rows.Next() {
		var id, label string
		var createdAt, expiresAt, isActive any
		if err := rows.Scan(&id, &label, &createdAt, &expiresAt, &isActive); err != nil {
			continue
		}
		tokens = append(tokens, map[string]any{
			"id": id, "label": label,
			"created_at": createdAt, "expires_at": expiresAt, "is_active": isActive,
		})
	}
	jsonOK(w, tokens)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	tokenID := chi.URLParam(r, "tokenID")

	tag, err := s.db.Exec(r.Context(),
		`UPDATE exit_node_tokens SET is_active = false WHERE id = $1`, tokenID,
	)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- proxy credentials ---

func (s *Server) createProxyCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		http.Error(w, "username and password required", http.StatusBadRequest)
		return
	}

	var id string
	err := s.db.QueryRow(r.Context(),
		`INSERT INTO proxy_credentials (username, password_hash)
		 VALUES ($1, crypt($2, gen_salt('bf'))) RETURNING id`,
		body.Username, body.Password,
	).Scan(&id)
	if err != nil {
		http.Error(w, "failed to create credential", http.StatusInternalServerError)
		log.Printf("createProxyCredential: %v", err)
		return
	}

	jsonOK(w, map[string]any{"id": id, "username": body.Username})
}

func (s *Server) listProxyCredentials(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(),
		`SELECT id, username, created_at, is_active FROM proxy_credentials ORDER BY created_at DESC`,
	)
	if err != nil {
		http.Error(w, "failed to list credentials", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var creds []map[string]any
	for rows.Next() {
		var id, username string
		var createdAt, isActive any
		if err := rows.Scan(&id, &username, &createdAt, &isActive); err != nil {
			continue
		}
		creds = append(creds, map[string]any{
			"id": id, "username": username,
			"created_at": createdAt, "is_active": isActive,
		})
	}
	jsonOK(w, creds)
}

func (s *Server) revokeProxyCredential(w http.ResponseWriter, r *http.Request) {
	credID := chi.URLParam(r, "credID")

	tag, err := s.db.Exec(r.Context(),
		`UPDATE proxy_credentials SET is_active = false WHERE id = $1`, credID,
	)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, "credential not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
