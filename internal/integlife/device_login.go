package integlife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	issuedAt                time.Time
}

var (
	ErrDeviceDenied  = errors.New("device authorization denied")
	ErrDeviceExpired = errors.New("device authorization expired; run life login again")
)

func validVerificationURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && (u.Scheme == "https" ||
		(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")))
}

func (c *Client) RequestDeviceCode(ctx context.Context, clientName string) (DeviceAuthorization, error) {
	var code DeviceAuthorization
	status, err := c.devicePost(ctx, "/cli/device/code", map[string]string{"client_name": clientName}, &code)
	if err != nil {
		return code, err
	}
	if status != http.StatusOK {
		return DeviceAuthorization{}, fmt.Errorf("device code request http %d", status)
	}
	if code.DeviceCode == "" || code.UserCode == "" || code.ExpiresIn <= 0 || code.ExpiresIn > 86400 || code.Interval < 0 || code.Interval > code.ExpiresIn || !validVerificationURL(code.VerificationURI) ||
		(code.VerificationURIComplete != "" && !validVerificationURL(code.VerificationURIComplete)) {
		return DeviceAuthorization{}, errors.New("invalid device authorization response")
	}
	if code.Interval == 0 {
		code.Interval = 5
	}
	code.issuedAt = time.Now()
	return code, nil
}

// devicePost never attaches the saved token or follows a redirect with a device secret.
func (c *Client) devicePost(ctx context.Context, path string, payload any, result any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, errors.New("encode device request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("build device request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := *c.httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send device request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return resp.StatusCode, fmt.Errorf("device request http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		return resp.StatusCode, errors.New("invalid device response JSON")
	}
	return resp.StatusCode, nil
}

func waitDevicePoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) PollDeviceToken(ctx context.Context, code DeviceAuthorization) (string, error) {
	return c.pollDeviceToken(ctx, code, waitDevicePoll)
}

func (c *Client) pollDeviceToken(ctx context.Context, code DeviceAuthorization, wait func(context.Context, time.Duration) error) (string, error) {
	if code.ExpiresIn <= 0 {
		return "", ErrDeviceExpired
	}
	issuedAt := code.issuedAt
	if issuedAt.IsZero() {
		issuedAt = time.Now()
	}
	pollCtx, cancel := context.WithDeadline(ctx, issuedAt.Add(time.Duration(code.ExpiresIn)*time.Second))
	defer cancel()
	interval := time.Duration(code.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		if err := wait(pollCtx, interval); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return "", ErrDeviceExpired
			}
			return "", err
		}
		var response struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			Error       string `json:"error"`
		}
		status, err := c.devicePost(pollCtx, "/cli/device/token", map[string]string{"device_code": code.DeviceCode}, &response)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if pollCtx.Err() != nil {
				return "", ErrDeviceExpired
			}
			// Back off on a transport timeout as required by RFC 8628.
			var timeout interface{ Timeout() bool }
			if errors.As(err, &timeout) && timeout.Timeout() {
				interval *= 2
				continue
			}
			return "", err
		}
		if status == http.StatusOK {
			if response.AccessToken == "" || !strings.EqualFold(response.TokenType, "Bearer") || response.Error != "" {
				return "", errors.New("invalid device token response")
			}
			return response.AccessToken, nil
		}
		switch response.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return "", ErrDeviceDenied
		case "expired_token":
			return "", ErrDeviceExpired
		case "invalid_grant":
			return "", errors.New("invalid or consumed device code; run life login again")
		default:
			return "", errors.New("device authorization failed")
		}
	}
}

// AuthStatus verifies the configured credential without returning or printing it.
func (c *Client) AuthStatus(ctx context.Context) (bool, error) {
	if !c.CanSync() {
		return false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/api/users/me", nil)
	if err != nil {
		return false, errors.New("build auth status request")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	client := *c.httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return false, errors.New("cannot check login status; API unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("auth status http %d", resp.StatusCode)
	}
	return true, nil
}
