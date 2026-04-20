package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testAdminToken = "test-admin-token"

// newTestAPI returns a GatewayAPI wired to a pool, session store, and router.
func newTestAPI(t *testing.T) (*GatewayAPI, *Pool, *SessionStore) {
	t.Helper()
	p := &Pool{}
	store := NewSessionStore(context.Background(), time.Hour)
	router := NewRouter(p, NewCredentialLimiter(1000), nil, store)
	api := &GatewayAPI{pool: p, sessions: store, router: router, token: testAdminToken}
	return api, p, store
}

func authHeader() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testAdminToken)
	return h
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// --- auth middleware ---

func TestGatewayAPI_Auth_MissingToken(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := doRequest(t, handler, "GET", "/", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestGatewayAPI_Auth_WrongToken(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h := http.Header{}
	h.Set("Authorization", "Bearer wrong")
	w := doRequest(t, handler, "GET", "/", nil, h)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestGatewayAPI_Auth_CorrectToken(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := doRequest(t, handler, "GET", "/", nil, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- GET /api/v1/exits ---

func TestGatewayAPI_Exits_EmptyPool(t *testing.T) {
	api, _, _ := newTestAPI(t)
	w := doRequest(t, http.HandlerFunc(api.exits), "GET", "/api/v1/exits", nil, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []exitNodeView
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty array, got %d entries", len(result))
	}
}

func TestGatewayAPI_Exits_ReturnsNodeFields(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "1.2.3.4")
	se.country = "us"
	se.nodeType = "residential"
	se.activeStreams.Store(3)
	p.add(se)

	w := doRequest(t, http.HandlerFunc(api.exits), "GET", "/api/v1/exits", nil, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result []exitNodeView
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	v := result[0]
	if v.IP != "1.2.3.4" {
		t.Errorf("IP: got %q, want %q", v.IP, "1.2.3.4")
	}
	if v.Country != "us" {
		t.Errorf("Country: got %q, want %q", v.Country, "us")
	}
	if v.NodeType != "residential" {
		t.Errorf("NodeType: got %q, want %q", v.NodeType, "residential")
	}
	if v.ActiveStreams != 3 {
		t.Errorf("ActiveStreams: got %d, want 3", v.ActiveStreams)
	}
	if v.Health != "healthy" {
		t.Errorf("Health: got %q, want healthy", v.Health)
	}
	if v.ID == "" {
		t.Error("ID must not be empty")
	}
}

func TestGatewayAPI_Exits_DegradedNode(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "1.2.3.4")
	// record enough failures to mark as degraded
	for range healthWindowSize {
		se.health.record(false)
	}
	p.add(se)

	w := doRequest(t, http.HandlerFunc(api.exits), "GET", "/api/v1/exits", nil, authHeader())
	var result []exitNodeView
	_ = json.NewDecoder(w.Body).Decode(&result)
	if len(result) != 1 || result[0].Health != "degraded" {
		t.Fatalf("expected degraded node, got %+v", result)
	}
}

func TestGatewayAPI_Exits_OmitsMetadataWhenEmpty(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "1.2.3.4")
	p.add(se)

	w := doRequest(t, http.HandlerFunc(api.exits), "GET", "/api/v1/exits", nil, authHeader())
	// decode as raw map to check that omitempty fields are absent
	var raw []map[string]any
	_ = json.NewDecoder(w.Body).Decode(&raw)
	if _, ok := raw[0]["country"]; ok {
		t.Error("country field must be omitted when empty")
	}
	if _, ok := raw[0]["node_type"]; ok {
		t.Error("node_type field must be omitted when empty")
	}
}

// --- GET /api/v1/stats ---

func TestGatewayAPI_Stats_EmptyPool(t *testing.T) {
	api, _, _ := newTestAPI(t)
	w := doRequest(t, http.HandlerFunc(api.stats), "GET", "/api/v1/stats", nil, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result["exitnodes"].(float64) != 0 {
		t.Errorf("expected exitnodes=0, got %v", result["exitnodes"])
	}
	if result["active_streams"].(float64) != 0 {
		t.Errorf("expected active_streams=0, got %v", result["active_streams"])
	}
}

func TestGatewayAPI_Stats_WithNodes(t *testing.T) {
	api, p, _ := newTestAPI(t)
	for i := range 3 {
		client, _ := yamuxPair(t)
		se := newSessionEntry(client, fmt.Sprintf("10.0.0.%d", i+1))
		se.activeStreams.Store(int32(i + 1)) // 1+2+3 = 6 total
		p.add(se)
	}

	w := doRequest(t, http.HandlerFunc(api.stats), "GET", "/api/v1/stats", nil, authHeader())
	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result["exitnodes"].(float64) != 3 {
		t.Errorf("expected exitnodes=3, got %v", result["exitnodes"])
	}
	if result["active_streams"].(float64) != 6 {
		t.Errorf("expected active_streams=6, got %v", result["active_streams"])
	}
}

// --- POST /api/v1/feedback ---

func TestGatewayAPI_Feedback_RecordsSuccess(t *testing.T) {
	api, _, store := newTestAPI(t)
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	store.Set("tok1", se)

	body, _ := json.Marshal(map[string]any{"session_token": "tok1", "success": true})
	w := doRequest(t, http.HandlerFunc(api.feedback), "POST", "/api/v1/feedback", body, authHeader())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGatewayAPI_Feedback_RecordsFailure(t *testing.T) {
	api, _, store := newTestAPI(t)
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	// prime with enough observations so failure rate is observable
	for range healthWindowSize {
		se.health.record(true)
	}
	store.Set("tok1", se)
	if se.health.isDegraded() {
		t.Fatal("should be healthy before recording failures")
	}

	for range healthWindowSize {
		body, _ := json.Marshal(map[string]any{"session_token": "tok1", "success": false})
		w := doRequest(t, http.HandlerFunc(api.feedback), "POST", "/api/v1/feedback", body, authHeader())
		if w.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", w.Code)
		}
	}
	if !se.health.isDegraded() {
		t.Fatal("expected node to be degraded after many failure feedbacks")
	}
}

func TestGatewayAPI_Feedback_SessionNotFound(t *testing.T) {
	api, _, _ := newTestAPI(t)
	body, _ := json.Marshal(map[string]any{"session_token": "missing", "success": true})
	w := doRequest(t, http.HandlerFunc(api.feedback), "POST", "/api/v1/feedback", body, authHeader())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGatewayAPI_Feedback_MissingToken(t *testing.T) {
	api, _, _ := newTestAPI(t)
	body, _ := json.Marshal(map[string]any{"success": true})
	w := doRequest(t, http.HandlerFunc(api.feedback), "POST", "/api/v1/feedback", body, authHeader())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGatewayAPI_Feedback_BadJSON(t *testing.T) {
	api, _, _ := newTestAPI(t)
	w := doRequest(t, http.HandlerFunc(api.feedback), "POST", "/api/v1/feedback", []byte("not-json"), authHeader())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- Register (disabled when token empty) ---

func TestGatewayAPI_Register_DisabledWhenNoToken(t *testing.T) {
	api := &GatewayAPI{pool: &Pool{}, sessions: NewSessionStore(context.Background(), time.Hour), token: ""}
	mux := http.NewServeMux()
	api.Register(mux)
	// no routes registered — any request should 404
	w := doRequest(t, mux, "GET", "/api/v1/exits", nil, authHeader())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when API is disabled, got %d", w.Code)
	}
}

func TestGatewayAPI_Register_RoutesRegisteredWithToken(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	p.add(newSessionEntry(client, "1.2.3.4"))
	mux := http.NewServeMux()
	api.Register(mux)

	w := doRequest(t, mux, "GET", "/api/v1/exits", nil, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- POST /api/v1/sessions ---

func TestGatewayAPI_CreateSession_Basic(t *testing.T) {
	api, p, store := newTestAPI(t)
	client, server := yamuxPair(t)
	serveStreams(server)
	p.add(newSessionEntry(client, "1.2.3.4"))

	body, _ := json.Marshal(map[string]any{})
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	token, ok := result["token"].(string)
	if !ok || token == "" {
		t.Fatalf("expected non-empty token, got %v", result["token"])
	}
	if result["expires_at"] == "" {
		t.Error("expected non-empty expires_at")
	}
	if result["exit"] == nil {
		t.Error("expected exit field")
	}

	// token must be stored in session store
	if store.Get(token) == nil {
		t.Fatal("token must be stored in SessionStore after CreateSession")
	}
}

func TestGatewayAPI_CreateSession_EmptyBody(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	p.add(newSessionEntry(client, "1.2.3.4"))

	// empty body (no JSON) must succeed with defaults
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", nil, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGatewayAPI_CreateSession_CustomTTL(t *testing.T) {
	api, p, store := newTestAPI(t)
	client, _ := yamuxPair(t)
	p.add(newSessionEntry(client, "1.2.3.4"))

	ttlSeconds := 300
	body, _ := json.Marshal(map[string]any{"ttl_seconds": ttlSeconds})
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	token := result["token"].(string)

	// TTL should be ~5 minutes — check the record's expiry is within 10s of expected
	store.mu.Lock()
	rec := store.records[token]
	store.mu.Unlock()
	if rec == nil {
		t.Fatal("token not in store")
	}
	expectedExpiry := time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	diff := rec.expiresAt.Sub(expectedExpiry)
	if diff < -10*time.Second || diff > 10*time.Second {
		t.Fatalf("expiry %v too far from expected %v (diff %v)", rec.expiresAt, expectedExpiry, diff)
	}
}

func TestGatewayAPI_CreateSession_TTLCappedAtMax(t *testing.T) {
	api, p, store := newTestAPI(t)
	client, _ := yamuxPair(t)
	p.add(newSessionEntry(client, "1.2.3.4"))

	body, _ := json.Marshal(map[string]any{"ttl_seconds": int(48 * time.Hour / time.Second)})
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	token := result["token"].(string)

	store.mu.Lock()
	rec := store.records[token]
	store.mu.Unlock()

	maxExpiry := time.Now().Add(maxSessionTTL)
	if rec.expiresAt.After(maxExpiry.Add(5 * time.Second)) {
		t.Fatalf("TTL was not capped: expiry %v exceeds max %v", rec.expiresAt, maxExpiry)
	}
}

func TestGatewayAPI_CreateSession_WithCountryConstraint(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client1, _ := yamuxPair(t)
	client2, _ := yamuxPair(t)
	se1 := newSessionEntry(client1, "1.2.3.4")
	se2 := newSessionEntry(client2, "5.6.7.8")
	se1.country = "us"
	se2.country = "gb"
	p.add(se1)
	p.add(se2)

	body, _ := json.Marshal(map[string]any{"country": "gb"})
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	exit := result["exit"].(map[string]any)
	if exit["ip"] != "5.6.7.8" {
		t.Fatalf("expected gb node (5.6.7.8), got ip=%v", exit["ip"])
	}
}

func TestGatewayAPI_CreateSession_NoEligibleNodes(t *testing.T) {
	api, _, _ := newTestAPI(t) // empty pool
	body, _ := json.Marshal(map[string]any{})
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestGatewayAPI_CreateSession_NoMatchForConstraint(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "1.2.3.4")
	se.country = "us"
	p.add(se)

	body, _ := json.Marshal(map[string]any{"country": "jp"})
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no node matches constraint, got %d", w.Code)
	}
}

func TestGatewayAPI_CreateSession_TokenUnique(t *testing.T) {
	api, p, _ := newTestAPI(t)
	client, _ := yamuxPair(t)
	p.add(newSessionEntry(client, "1.2.3.4"))

	tokens := map[string]bool{}
	for range 20 {
		body, _ := json.Marshal(map[string]any{})
		w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", body, authHeader())
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected error: %d", w.Code)
		}
		var result map[string]any
		_ = json.NewDecoder(w.Body).Decode(&result)
		tok := result["token"].(string)
		if tokens[tok] {
			t.Fatalf("duplicate token generated: %s", tok)
		}
		tokens[tok] = true
	}
}

func TestGatewayAPI_CreateSession_BadJSON(t *testing.T) {
	api, _, _ := newTestAPI(t)
	w := doRequest(t, http.HandlerFunc(api.createSession), "POST", "/api/v1/sessions", []byte("not-json"), authHeader())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", w.Code)
	}
}
