package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

type Config struct {
	GatewayURL string `json:"gateway_url"`
	Token      string `json:"token"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ambush", "exitnode.json")
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Token == "" {
		return nil, errors.New("config missing token")
	}
	return &cfg, nil
}

func saveConfig(cfg *Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func runSetup() (*Config, error) {
	cfg := &Config{GatewayURL: "ws://localhost:8080"}

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Gateway URL").
				Placeholder("ws://localhost:8080").
				Value(&cfg.GatewayURL),
			huh.NewInput().
				Title("Token").
				Placeholder("paste your exit node token").
				EchoMode(huh.EchoModePassword).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("token cannot be empty")
					}
					return nil
				}).
				Value(&cfg.Token),
		),
	).WithTheme(huh.ThemeCatppuccin()).Run()
	if err != nil {
		return nil, err
	}

	cfg.Token = strings.TrimSpace(cfg.Token)

	if err := saveConfig(cfg); err != nil {
		return nil, fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Println("Config saved to", configPath())
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		cfg, err = runSetup()
		if err != nil {
			log.Fatal(err)
		}
	}

	log.Printf("connecting to %s", cfg.GatewayURL)
	for {
		if err := connect(cfg.GatewayURL, cfg.Token); err != nil {
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
