package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// machineToken is a credential taken from the environment rather than from an
// interactive browser sign-in.
//
// CI has no browser, and `ddc agent deploy` is meant to be the same command in
// a pipeline as on a laptop. Without this, "run it in CI" would mean a second
// code path, which is exactly what this design is trying to avoid.
type machineToken struct {
	value   string
	expires time.Time
}

var (
	machineOnce  sync.Once
	machineCache *machineToken
	machineMu    sync.Mutex
)

// MachineCredentialsPresent reports whether the environment carries a
// non-interactive credential, so callers can skip the browser sign-in.
func MachineCredentialsPresent() bool {
	if os.Getenv("DDC_TOKEN") != "" {
		return true
	}
	return os.Getenv("DDC_CLIENT_ID_SECRET") != "" ||
		(os.Getenv("DDC_SERVICE_CLIENT_ID") != "" && os.Getenv("DDC_SERVICE_CLIENT_SECRET") != "")
}

// machineTokenSource returns a token from DDC_TOKEN, or fetches one with the
// client-credentials grant. It returns "" when no machine credential is set,
// which tells the caller to fall back to the interactive session.
func machineTokenSource(ctx context.Context, cfg Config) (string, error) {
	if direct := os.Getenv("DDC_TOKEN"); direct != "" {
		return direct, nil
	}
	clientID := os.Getenv("DDC_SERVICE_CLIENT_ID")
	secret := os.Getenv("DDC_SERVICE_CLIENT_SECRET")
	if clientID == "" || secret == "" {
		return "", nil
	}

	machineMu.Lock()
	defer machineMu.Unlock()
	if machineCache != nil && time.Now().Before(machineCache.expires) {
		return machineCache.value, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	endpoint := strings.TrimRight(cfg.Issuer, "/") + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("client-credentials request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("client-credentials request returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("client-credentials response carried no access token")
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Minute
	}
	machineCache = &machineToken{value: payload.AccessToken, expires: time.Now().Add(lifetime - 30*time.Second)}
	return machineCache.value, nil
}
