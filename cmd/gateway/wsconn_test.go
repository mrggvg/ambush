package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsConnPair dials a test WebSocket server and returns the client-side wsConn
// together with the server-side *websocket.Conn for driving the other end.
func wsConnPair(t *testing.T) (client *wsConn, server *websocket.Conn) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ready := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ready <- conn
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	wsc, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { wsc.Close() })

	server = <-ready
	t.Cleanup(func() { server.Close() })
	client = &wsConn{conn: wsc}
	return client, server
}

// --- SetDeadline ---

func TestWsConn_SetDeadline_BlocksRead(t *testing.T) {
	client, _ := wsConnPair(t)

	// deadline in the past → any read must fail immediately
	_ = client.SetDeadline(time.Now().Add(-time.Second))

	buf := make([]byte, 1)
	_, err := client.Read(buf)
	if err == nil {
		t.Fatal("expected read to fail after past deadline, got nil")
	}
}

func TestWsConn_SetDeadline_BlocksWrite(t *testing.T) {
	client, _ := wsConnPair(t)

	_ = client.SetDeadline(time.Now().Add(-time.Second))

	_, err := client.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected write to fail after past deadline, got nil")
	}
}

func TestWsConn_SetReadDeadline_BlocksReadOnly(t *testing.T) {
	client, server := wsConnPair(t)

	_ = client.SetReadDeadline(time.Now().Add(-time.Second))

	buf := make([]byte, 1)
	_, err := client.Read(buf)
	if err == nil {
		t.Fatal("expected read to fail after past read deadline")
	}

	// write must still work
	go func() { server.ReadMessage() }() // drain so write doesn't block
	if _, err := client.Write([]byte("ok")); err != nil {
		t.Fatalf("write should succeed after read-only deadline: %v", err)
	}
}

// --- Read / Write ---

func TestWsConn_ReadWrite(t *testing.T) {
	client, server := wsConnPair(t)

	want := "hello wsConn"
	go func() {
		server.WriteMessage(websocket.BinaryMessage, []byte(want))
	}()

	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

func TestWsConn_WriteRead(t *testing.T) {
	client, server := wsConnPair(t)

	want := "hello server"
	done := make(chan string, 1)
	go func() {
		_, msg, err := server.ReadMessage()
		if err != nil {
			done <- ""
			return
		}
		done <- string(msg)
	}()

	if _, err := client.Write([]byte(want)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := <-done; got != want {
		t.Fatalf("server read %q, want %q", got, want)
	}
}

func TestWsConn_ReadAcrossMultipleMessages(t *testing.T) {
	client, server := wsConnPair(t)

	// send two messages; Read must reassemble them across NextReader calls
	go func() {
		server.WriteMessage(websocket.BinaryMessage, []byte("foo"))
		server.WriteMessage(websocket.BinaryMessage, []byte("bar"))
	}()

	got := make([]byte, 0, 6)
	buf := make([]byte, 64)
	for len(got) < 6 {
		n, err := client.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "foobar" {
		t.Fatalf("got %q, want %q", got, "foobar")
	}
}

// --- Addr ---

func TestWsConn_Addrs(t *testing.T) {
	client, _ := wsConnPair(t)
	if client.LocalAddr() == nil {
		t.Fatal("LocalAddr returned nil")
	}
	if client.RemoteAddr() == nil {
		t.Fatal("RemoteAddr returned nil")
	}
}
