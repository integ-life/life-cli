package integlife

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestDevicePolling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		errors    []string
		wantErr   error
		wantWaits []time.Duration
	}{
		{"pending_success", []string{"authorization_pending", ""}, nil, []time.Duration{5 * time.Second, 5 * time.Second}},
		{"slow_down_success", []string{"authorization_pending", "slow_down", "authorization_pending", "slow_down", ""}, nil, []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second, 15 * time.Second}},
		{"denied", []string{"access_denied"}, ErrDeviceDenied, []time.Duration{5 * time.Second}},
		{"expired", []string{"expired_token"}, ErrDeviceExpired, []time.Duration{5 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poll := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/cli/device/token" {
					t.Error("unexpected polling request")
					w.WriteHeader(404)
					return
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("saved credential sent to public device endpoint")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["device_code"] != "fake-device-secret" {
					t.Error("wrong device payload")
				}
				if poll >= len(tc.errors) {
					t.Error("unexpected extra poll")
					w.WriteHeader(500)
					return
				}
				code := tc.errors[poll]
				poll++
				if code != "" {
					w.WriteHeader(400)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok_fake_test_secret", "token_type": "Bearer"})
			}))
			defer server.Close()
			var waits []time.Duration
			wait := func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
			client := NewClient(server.URL, "fake-existing-credential", time.Second)
			secret, err := client.pollDeviceToken(context.Background(), DeviceAuthorization{DeviceCode: "fake-device-secret", ExpiresIn: 600, Interval: 5}, wait)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("wrong polling outcome: %v", err)
			}
			if tc.wantErr == nil && secret != "tok_fake_test_secret" {
				t.Error("wrong issued credential")
			}
			if tc.wantErr != nil && secret != "" {
				t.Error("credential returned after failed authorization")
			}
			if !reflect.DeepEqual(waits, tc.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tc.wantWaits)
			}
		})
	}
}

func TestRequestDeviceCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cli/device/code" || r.Method != "POST" {
			t.Error("unexpected request")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("saved credential sent to code endpoint")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_name"] != "life-cli on fake-pi" {
			t.Error("missing client name")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "fake-device-secret", "user_code": "ABCD-EFGH",
			"verification_uri": "https://app.example.test/#/device", "verification_uri_complete": "https://app.example.test/#/device?code=ABCD-EFGH", "expires_in": 600, "interval": 5})
	}))
	defer server.Close()
	code, err := NewClient(server.URL, "fake-existing", time.Second).RequestDeviceCode(context.Background(), "life-cli on fake-pi")
	if err != nil {
		t.Fatal(err)
	}
	if code.UserCode != "ABCD-EFGH" || code.Interval != 5 || code.issuedAt.IsZero() {
		t.Error("invalid code response")
	}
}

func TestDevicePollingCancellationAndLocalExpiry(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.PollDeviceToken(ctx, DeviceAuthorization{ExpiresIn: 600, Interval: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	_, err = client.PollDeviceToken(context.Background(), DeviceAuthorization{ExpiresIn: 1, issuedAt: time.Now().Add(-time.Minute), Interval: 5})
	if !errors.Is(err, ErrDeviceExpired) {
		t.Fatalf("local expiry: %v", err)
	}
}

func TestDevicePollingRejectsInvalidGrantAndMalformedSuccess(t *testing.T) {
	for _, body := range []string{`{"error":"invalid_grant"}`, `{"access_token":"tok_fake_test_secret"}`, `{"token_type":"Bearer"}`, `{"error":"unknown_error"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if body[2:7] == "error" {
				w.WriteHeader(400)
			}
			_, _ = w.Write([]byte(body))
		}))
		_, err := NewClient(server.URL, "", time.Second).pollDeviceToken(context.Background(), DeviceAuthorization{DeviceCode: "fake", ExpiresIn: 600, Interval: 5}, func(context.Context, time.Duration) error { return nil })
		server.Close()
		if err == nil {
			t.Error("accepted invalid token response")
		}
	}
}

func TestDeviceRequestDoesNotFollowRedirect(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { followed = true; w.WriteHeader(500) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	_, err := NewClient(server.URL, "", time.Second).RequestDeviceCode(context.Background(), "fake")
	if err == nil || followed {
		t.Error("device request followed redirect")
	}
}

func TestAuthStatus(t *testing.T) {
	for _, status := range []int{200, 401, 403, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/users/me" || r.Header.Get("Authorization") != "Bearer tok_fake_status" {
				t.Error("wrong auth status request")
			}
			w.WriteHeader(status)
		}))
		loggedIn, err := NewClient(server.URL, "tok_fake_status", time.Second).AuthStatus(context.Background())
		server.Close()
		if loggedIn != (status == 200) || (err != nil) != (status == 500) {
			t.Errorf("wrong auth status for http %d", status)
		}
	}
	loggedIn, err := NewClient("http://127.0.0.1:1", "", time.Second).AuthStatus(context.Background())
	if loggedIn || err != nil {
		t.Error("missing credential should be logged out without network")
	}
}
