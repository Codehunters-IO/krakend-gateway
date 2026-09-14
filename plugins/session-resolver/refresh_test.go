package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

func TestRefresh(t *testing.T) {
	addr := startValkey(t)
	sid := "F123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+5, now+36000)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Header.Get("X-Internal-Secret") != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(req.URL.Path, sid) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The new token must land in Valkey, exactly as auth-bff does it.
		hset := client.B().Hset().Key("v1:session:"+sid).FieldValue().FieldValue("access_token", "fresh.jwt").Build()
		client.Do(req.Context(), hset)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh.jwt"}`))
	}))
	defer server.Close()

	cfg := &pluginConfig{
		ValkeyAddr:            addr,
		KeyPrefix:             "v1:",
		ValkeyTimeoutMs:       500,
		IdleTTLSeconds:        1800,
		RefreshURL:            server.URL + "/internal/sessions/{sid}/refresh",
		RefreshTimeoutMs:      2000,
		RefreshLockTTLSeconds: 5,
		InternalSecret:        "s3cret",
	}
	s, err := newStore(cfg)
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	r := newRefresher(cfg, s)

	t.Run("returns the refreshed access token", func(t *testing.T) {
		token, err := r.refresh(context.Background(), sid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "fresh.jwt" {
			t.Errorf("token = %q, want fresh.jwt", token)
		}
	})

	t.Run("only one of many concurrent callers hits auth-bff", func(t *testing.T) {
		client.Do(context.Background(), client.B().Del().Key("v1:lock:refresh:"+sid).Build())
		calls.Store(0)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = r.refresh(context.Background(), sid)
			}()
		}
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("auth-bff called %d times, want exactly 1", got)
		}
	})
}

// TestRefreshFailureModes gives each subtest its own sid rather than sharing
// one. acquire() never explicitly releases the refresh lock — it decays via
// lockTTL — so the winner of the 401 subtest would otherwise still hold the
// lock when the 5xx subtest ran immediately after it, sending that subtest
// down the wait-for-other-refresh path instead of ever reaching its own
// mock server. That is a real defect in the brief's original shared-sid
// version: the 5xx branch it claims to exercise is unreachable under it,
// and the assertion fails not because the plain-error path is wrong but
// because it was never taken. Separate sids restore test isolation without
// changing what either subtest asserts.
func TestRefreshFailureModes(t *testing.T) {
	addr := startValkey(t)
	now := time.Now().Unix()

	t.Run("401 from auth-bff means the session is gone", func(t *testing.T) {
		sid := "G123456789012345678901234567890123456789012"
		seed(t, addr, sid, now+5, now+36000)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		_, err = r.refresh(context.Background(), sid)
		if !errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want errSessionGone", err)
		}
	})

	t.Run("5xx from auth-bff is a plain error", func(t *testing.T) {
		sid := "H323456789012345678901234567890123456789012"
		seed(t, addr, sid, now+5, now+36000)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		_, err = r.refresh(context.Background(), sid)
		if err == nil || errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want a non-session-gone error", err)
		}
	})
}

// TestRefreshAbsExpCeiling pins the ordering constraint carried from Tasks 2
// and 3: abs_exp is a hard stop, checked BEFORE the lock and BEFORE auth-bff
// is ever contacted. Neither earlier task had a comparison point for this —
// decide() carries no expiry fields and store.load() returns raw values — so
// refresh() is where the ceiling first gets an enforcement point.
func TestRefreshAbsExpCeiling(t *testing.T) {
	addr := startValkey(t)

	t.Run("session past its absolute expiry is refused before any auth-bff call or lock", func(t *testing.T) {
		sid := "N123456789012345678901234567890123456789012"
		now := time.Now().Unix()
		seed(t, addr, sid, now+5, now-1) // abs_exp already passed

		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"access_token":"should-not-happen"}`))
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		token, err := r.refresh(context.Background(), sid)
		if !errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want errSessionGone", err)
		}
		if token != "" {
			t.Errorf("token = %q, want empty on a ceiling refusal", token)
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("auth-bff called %d times, want 0 — abs_exp ceiling must be checked before the HTTP call", got)
		}

		n, err := s.client.Do(context.Background(), s.client.B().Exists().Key("v1:lock:refresh:"+sid).Build()).AsInt64()
		if err != nil {
			t.Fatalf("EXISTS failed: %v", err)
		}
		if n != 0 {
			t.Errorf("a refresh lock was created for a session past its abs_exp ceiling")
		}
	})

	t.Run("a corrupted abs_exp field (parses to zero) fails closed, not open", func(t *testing.T) {
		sid := "P123456789012345678901234567890123456789012"
		seedMalformedExpiry(t, addr, sid)

		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		_, err = r.refresh(context.Background(), sid)
		if !errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want errSessionGone", err)
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("auth-bff called %d times, want 0 — a corrupted abs_exp must not be read as \"no ceiling\"", got)
		}
	})

	t.Run("missing session is refused as session gone, not a store error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		_, err = r.refresh(context.Background(), "Q123456789012345678901234567890123456789012")
		if !errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want errSessionGone", err)
		}
	})
}

func refreshConfig(valkeyAddr, serverURL string) *pluginConfig {
	return &pluginConfig{
		ValkeyAddr:            valkeyAddr,
		KeyPrefix:             "v1:",
		ValkeyTimeoutMs:       500,
		IdleTTLSeconds:        1800,
		RefreshURL:            serverURL + "/internal/sessions/{sid}/refresh",
		RefreshTimeoutMs:      2000,
		RefreshLockTTLSeconds: 5,
		InternalSecret:        "s3cret",
	}
}

// seedMalformedExpiry writes a session hash whose exp/abs_exp fields are
// non-numeric, mirroring TestStoreLoadWithMalformedExpiryFields in
// session_test.go. asInt64 swallows the parse failure and reports 0 for
// both fields — this pins that the refresh ceiling treats that zero as
// "long expired", not as "no ceiling set".
func seedMalformedExpiry(t *testing.T, addr, sid string) {
	t.Helper()
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("failed to connect to valkey: %v", err)
	}
	t.Cleanup(client.Close)

	ctx := context.Background()
	key := "v1:session:" + sid
	hset := client.B().Hset().Key(key).FieldValue().
		FieldValue("ver", "1").
		FieldValue("sub", "user-1").
		FieldValue("kc_sid", "kc-1").
		FieldValue("access_token", "the.jwt").
		FieldValue("exp", "not-a-number").
		FieldValue("abs_exp", "also-not-a-number").
		FieldValue("refresh_token_enc", "k1.iv.cipher").
		FieldValue("id_token_enc", "k1.iv.cipher").
		FieldValue("created_at", "0").
		Build()
	if err := client.Do(ctx, hset).Error(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	if err := client.Do(ctx, client.B().Expire().Key(key).Seconds(1800).Build()).Error(); err != nil {
		t.Fatalf("expire failed: %v", err)
	}
}
