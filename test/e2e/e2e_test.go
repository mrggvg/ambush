package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/net/proxy"
)

const (
	testSocksUser = "e2e-user"
	testSocksPass = "e2e-pass"
	testToken     = "e2e-test-token-ambush-do-not-use-in-production"
)

// Binaries built once in TestMain, shared across all tests.
var (
	gatewayBin  string
	exitnodeBin string
)

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "ambush-e2e-bins-*")
	if err != nil {
		log.Fatalf("tmpdir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	gatewayBin = filepath.Join(tmpDir, "gateway")
	exitnodeBin = filepath.Join(tmpDir, "exitnode")

	root := projectRoot()
	if err := buildBin(root, "./cmd/gateway", gatewayBin); err != nil {
		log.Fatalf("build gateway: %v", err)
	}
	if err := buildBin(root, "./cmd/exitnode", exitnodeBin); err != nil {
		log.Fatalf("build exitnode: %v", err)
	}

	os.Exit(m.Run())
}

func TestE2ETrafficFlowsThroughExitNode(t *testing.T) {
	ctx := context.Background()

	// --- Postgres container ---
	pgc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:16-alpine",
			Env: map[string]string{
				"POSTGRES_DB":       "ambush_test",
				"POSTGRES_USER":     "ambush",
				"POSTGRES_PASSWORD": "ambush",
			},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgc.Terminate(ctx) })

	port, err := pgc.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatalf("get mapped port: %v", err)
	}
	dbURL := fmt.Sprintf("postgresql://ambush:ambush@localhost:%s/ambush_test?sslmode=disable", port.Port())

	// --- Apply schema and seed ---
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	t.Cleanup(pool.Close)

	applySchema(t, ctx, pool)
	seedTestData(t, ctx, pool)

	// --- Generate TLS certs ---
	caCertPEM, gwCertPEM, gwKeyPEM := genTestCerts(t)

	certFile := filepath.Join(t.TempDir(), "gateway.crt")
	keyFile := filepath.Join(t.TempDir(), "gateway.key")
	if err := os.WriteFile(certFile, gwCertPEM, 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, gwKeyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// --- Allocate ports ---
	gwPort := freePort(t)
	socksPort := freePort(t)

	// --- Start gateway with TLS ---
	gwCmd := exec.Command(gatewayBin)
	gwCmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		fmt.Sprintf("GATEWAY_ADDR=:%d", gwPort),
		fmt.Sprintf("SOCKS5_ADDR=:%d", socksPort),
		"TLS_CERT="+certFile,
		"TLS_KEY="+keyFile,
	)
	gwCmd.Stdout = os.Stderr
	gwCmd.Stderr = os.Stderr
	if err := gwCmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() { _ = gwCmd.Process.Kill() })

	// --- Start exitnode over wss:// ---
	enCmd := exec.Command(exitnodeBin)
	enCmd.Env = append(os.Environ(),
		fmt.Sprintf("AMBUSH_GATEWAY_URL=wss://127.0.0.1:%d", gwPort),
		"AMBUSH_TOKEN="+testToken,
		"AMBUSH_CA_CERT="+string(caCertPEM),
	)
	enCmd.Stdout = os.Stderr
	enCmd.Stderr = os.Stderr
	if err := enCmd.Start(); err != nil {
		t.Fatalf("start exitnode: %v", err)
	}
	t.Cleanup(func() { _ = enCmd.Process.Kill() })

	// --- Wait until exit node is in the pool ---
	// Health endpoint is now HTTPS; use an insecure client just for polling.
	healthURL := fmt.Sprintf("https://127.0.0.1:%d/health", gwPort)
	waitForExitNode(t, healthURL, 30*time.Second)

	// --- Local echo target server ---
	// The exit node dials this directly — no external internet dependency.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listener: %v", err)
	}
	echoAddr := echoLn.Addr().String()
	echoSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ambush-e2e-ok")
		}),
	}
	go func() { _ = echoSrv.Serve(echoLn) }()
	t.Cleanup(func() { _ = echoSrv.Close() })

	// --- Route a request through the SOCKS5 proxy ---
	dialer, err := proxy.SOCKS5("tcp",
		fmt.Sprintf("127.0.0.1:%d", socksPort),
		&proxy.Auth{User: testSocksUser, Password: testSocksPass},
		proxy.Direct,
	)
	if err != nil {
		t.Fatalf("create socks5 dialer: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
		Timeout: 15 * time.Second,
	}

	resp, err := client.Get("http://" + echoAddr)
	if err != nil {
		t.Fatalf("socks5 request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ambush-e2e-ok" {
		t.Fatalf("unexpected response body: %q", string(body))
	}
	t.Log("traffic flowed through gateway (TLS) → exit node → echo server")
}

// --- Helpers ---

// genTestCerts generates an in-memory CA and gateway cert for testing.
// Returns (caCertPEM, gatewayCertPEM, gatewayKeyPEM).
func genTestCerts(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Ambush Test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	gwKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen gateway key: %v", err)
	}
	gwTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ambush-gateway"},
		DNSNames:     []string{"ambush-gateway"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	gwDER, err := x509.CreateCertificate(rand.Reader, gwTmpl, caCert, &gwKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create gateway cert: %v", err)
	}

	gwKeyDER, err := x509.MarshalECPrivateKey(gwKey)
	if err != nil {
		t.Fatalf("marshal gateway key: %v", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	gwCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gwDER})
	gwKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: gwKeyDER})
	return caPEM, gwCertPEM, gwKeyPEM
}

func applySchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join(projectRoot(), "db", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
}

func seedTestData(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ('e2e-test-user') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	h := sha256.Sum256([]byte(testToken))
	if _, err := pool.Exec(ctx,
		`INSERT INTO exit_node_tokens (user_id, token_hash, label) VALUES ($1, $2, 'e2e-node')`,
		userID, hex.EncodeToString(h[:]),
	); err != nil {
		t.Fatalf("insert exit node token: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO proxy_credentials (username, password_hash) VALUES ($1, crypt($2, gen_salt('bf')))`,
		testSocksUser, testSocksPass,
	); err != nil {
		t.Fatalf("insert proxy credential: %v", err)
	}
}

// waitForExitNode polls the health endpoint (HTTPS, insecure client) until
// at least one exit node is registered in the pool.
func waitForExitNode(t *testing.T, healthURL string, timeout time.Duration) {
	t.Helper()
	insecure := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
		Timeout: 3 * time.Second,
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := insecure.Get(healthURL)
		if err == nil {
			var h struct {
				ExitNodes int `json:"exitnodes"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&h)
			_ = resp.Body.Close()
			if h.ExitNodes > 0 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("timed out waiting for exit node to appear in gateway pool")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func buildBin(root, pkg, out string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v\n%s", err, output)
	}
	return nil
}

func projectRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
