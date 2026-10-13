package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoginWorksWithoutTTYOrDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("INTEGLIFE_API_TOKEN", "")
	t.Setenv("INTEGLIFE_DB_PATH", filepath.Join(t.TempDir(), "missing-parent", "db.sqlite"))
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("SSH_CONNECTION", "fake-headless-session")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "fake-device", "user_code": "ABCD-EFGH", "verification_uri": "https://app.example.test/#/device", "verification_uri_complete": "https://app.example.test/#/device?code=ABCD-EFGH", "interval": 1, "expires_in": 10})
		case "/cli/device/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok_fake_headless_test", "token_type": "Bearer"})
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("INTEGLIFE_API_URL", server.URL)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin, originalStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, stdoutWrite
	t.Cleanup(func() { os.Stdin, os.Stdout = originalStdin, originalStdout })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = run(ctx, []string{"login"})
	stdoutWrite.Close()
	output, readErr := io.ReadAll(stdoutRead)
	stdoutRead.Close()
	os.Stdin, os.Stdout = originalStdin, originalStdout
	if err != nil || readErr != nil {
		t.Fatalf("headless login failed: %v / %v", err, readErr)
	}
	if !strings.Contains(string(output), "ABCD-EFGH") || !strings.Contains(string(output), "https://app.example.test/#/device?code=ABCD-EFGH") {
		t.Error("missing login instructions")
	}
	if strings.Contains(string(output), "tok_fake") || strings.Contains(string(output), "fake-device") {
		t.Error("secret printed to terminal")
	}
	tokenPath := filepath.Join(home, ".integlife", "token")
	saved, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "tok_fake_headless_test\n" {
		t.Error("wrong saved credential")
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Error("credential file is not private")
	}
	os.Stdin = stdin
	err = run(ctx, []string{"login", "--password"})
	os.Stdin = originalStdin
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Error("password fallback accepted non-TTY input")
	}
}

func TestBrowserSkippedOnSSH(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "fake-ssh-session")
	t.Setenv("DISPLAY", ":99")
	if browserCommand("https://app.example.test") != nil {
		t.Error("browser should not open on SSH")
	}
}
