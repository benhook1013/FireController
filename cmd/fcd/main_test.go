package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type output struct {
	sync.Mutex
	data  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (o *output) Write(data []byte) (int, error) {
	o.Lock()
	defer o.Unlock()
	n, err := o.data.Write(data)
	if bytes.Contains(o.data.Bytes(), []byte("\n")) {
		o.once.Do(func() { close(o.ready) })
	}
	return n, err
}
func (o *output) String() string { o.Lock(); defer o.Unlock(); return o.data.String() }

func TestExecutableBootIdentityAndGracefulRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	instance := ""
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		stdout := &output{ready: make(chan struct{})}
		stderr := &output{ready: make(chan struct{})}
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"--data-dir", dir, "--listen", "127.0.0.1:0"}, stdout, stderr) }()
		select {
		case <-stdout.ready:
		case code := <-done:
			cancel()
			t.Fatalf("startup stopped: code=%d diagnostics=%s", code, stderr.String())
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("startup deadline")
		}
		var started map[string]string
		if err := json.Unmarshal([]byte(stdout.String()), &started); err != nil {
			cancel()
			t.Fatal(err)
		}
		if instance == "" {
			instance = started["instance_id"]
		} else if instance != started["instance_id"] {
			cancel()
			t.Fatal("restart changed instance")
		}
		transport := &http.Transport{DisableKeepAlives: true}
		client := &http.Client{Timeout: time.Second, Transport: transport}
		response, err := client.Get("http://" + started["address"] + "/readyz")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			cancel()
			t.Fatal("not ready")
		}
		for _, route := range []string{
			"/api/v1/projects/untrusted/jobs",
			"/api/v1/projects/untrusted/inbox/untrusted",
		} {
			response, err = client.Get("http://" + started["address"] + route)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				cancel()
				t.Fatalf("domain route %s not registered with authentication: %d", route, response.StatusCode)
			}
		}
		data, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		token := strings.TrimSpace(string(data))
		req, _ := http.NewRequest("GET", "http://"+started["address"]+"/api/v1/instance", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err = client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte(instance)) {
			cancel()
			t.Fatal("authenticated instance inspection failed")
		}
		transport.CloseIdleConnections()
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("shutdown code=%d diagnostics=%s", code, stderr.String())
			}
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown deadline")
		}
		if strings.Contains(stdout.String()+stderr.String(), token) || stderr.String() != "" {
			t.Fatal("startup/shutdown emitted credential or diagnostics")
		}
	}
}

func TestConfigurationRejectsNonLoopbackBeforeCreatingState(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", "[::]:8080", "controller.example:8080", "127.0.0.1:65536", "127.0.0.1:bad"} {
		dir := filepath.Join(t.TempDir(), "absent")
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"--data-dir", dir, "--listen", address}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatal("unsafe listener accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid configuration created state")
		}
	}
}
