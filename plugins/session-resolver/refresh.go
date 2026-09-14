package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

var errSessionGone = errors.New("session gone")

type refresher struct {
	store    *store
	client   *http.Client
	url      string
	secret   string
	lockTTL  time.Duration
	timeout  time.Duration
	waitStep time.Duration
	maxWait  time.Duration
}

func newRefresher(cfg *pluginConfig, s *store) *refresher {
	timeout := time.Duration(cfg.RefreshTimeoutMs) * time.Millisecond
	return &refresher{
		store:    s,
		client:   &http.Client{Timeout: timeout},
		url:      cfg.RefreshURL,
		secret:   cfg.InternalSecret,
		lockTTL:  time.Duration(cfg.RefreshLockTTLSeconds) * time.Second,
		timeout:  timeout,
		waitStep: 50 * time.Millisecond,
		maxWait:  500 * time.Millisecond,
	}
}

// refresh serialises concurrent refreshes for one session behind a Valkey lock.
// The winner calls auth-bff; the losers wait and re-read the token it wrote.
//
// abs_exp is checked here, before the lock is ever touched and before
// auth-bff is ever contacted: a session past its absolute ceiling is dead
// even if a refresh would succeed, so neither the lock nor the HTTP call may
// be reached for it. Tasks 2 and 3 deliberately carry no comparison logic —
// decide() has no expiry fields and store.load() returns the raw values —
// so this is the first point in the plugin able to enforce that ordering.
func (r *refresher) refresh(ctx context.Context, sid string) (string, error) {
	data, err := r.store.load(ctx, sid)
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", errSessionGone
	}
	// asInt64 (session.go) silently zeroes a malformed abs_exp field. A
	// plain now >= AbsExp comparison then treats a corrupted ceiling as
	// long past, which is the fail-closed direction: 0 can never be a
	// legitimate future epoch, so the ceiling still bites.
	if time.Now().Unix() >= data.AbsExp {
		return "", errSessionGone
	}

	won, err := r.acquire(ctx, sid)
	if err != nil {
		return "", err
	}
	if !won {
		return r.waitForOtherRefresh(ctx, sid)
	}
	return r.callAuthBff(ctx, sid)
}

// acquire returns (true, nil) when this caller won the lock, and (false, nil)
// — not an error — when SET NX found the key already held. valkey-go reports
// a "not set" NX outcome as a nil reply, surfaced as the sentinel error
// valkey.Nil, so it must be checked before treating err as a real failure.
func (r *refresher) acquire(ctx context.Context, sid string) (bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, r.store.timeout)
	defer cancel()

	lockKey := r.store.prefix + "lock:refresh:" + sid
	cmd := r.store.client.B().Set().Key(lockKey).Value("1").Nx().Ex(r.lockTTL).Build()
	err := r.store.client.Do(lockCtx, cmd).Error()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *refresher) waitForOtherRefresh(ctx context.Context, sid string) (string, error) {
	deadline := time.Now().Add(r.maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(r.waitStep)

		data, err := r.store.load(ctx, sid)
		if err != nil {
			return "", err
		}
		if data == nil {
			return "", errSessionGone
		}
		if data.Exp > time.Now().Unix() {
			return data.AccessToken, nil
		}
	}
	return "", errors.New("timed out waiting for a concurrent refresh")
}

func (r *refresher) callAuthBff(ctx context.Context, sid string) (string, error) {
	url := strings.ReplaceAll(r.url, "{sid}", sid)

	reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Secret", r.secret)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", errSessionGone
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-bff returned status %d", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", errors.New("auth-bff returned an empty access token")
	}
	return body.AccessToken, nil
}
