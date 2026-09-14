package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	var mockExp atomic.Int64
	mockExp.Store(now + 5)
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
		// A real auth-bff round trip does a Keycloak token exchange — not
		// instantaneous. This delay is a permanent, deliberate part of the
		// model, not a test crutch: without it, an in-process mock replies
		// inside a loser's first 50ms poll every time, so the concurrency
		// subtest below could never actually exercise the wait branch —
		// reverting waitForOtherRefresh's condition to its old, broken form
		// would still pass. See the fix report for the mutation evidence
		// this produces against this exact arrangement.
		time.Sleep(150 * time.Millisecond)
		// The new token — and a strictly-later exp — must land in Valkey,
		// exactly as auth-bff does it: it rewrites the whole session hash
		// on every refresh, so a real refresh always advances exp. This is
		// what waitForOtherRefresh's losers actually wait on.
		newExp := mockExp.Add(300)
		hset := client.B().Hset().Key("v1:session:"+sid).FieldValue().
			FieldValue("access_token", "fresh.jwt").
			FieldValue("exp", strconv.FormatInt(newExp, 10)).
			Build()
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
		token, err := r.refresh(context.Background(), sid, now+5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "fresh.jwt" {
			t.Errorf("token = %q, want fresh.jwt", token)
		}
	})

	t.Run("only one of many concurrent callers hits auth-bff, and every caller gets the new token, not the stale one", func(t *testing.T) {
		// Reseed a known, distinctly-stale token/exp so this subtest's
		// assertions do not ride on the previous subtest's writes: that
		// subtest already left access_token = "fresh.jwt" in place, which
		// would make a losers-return-the-stale-token regression invisible
		// here if this subtest were left to inherit it.
		staleExp := now + 5
		reseed := client.B().Hset().Key("v1:session:"+sid).FieldValue().
			FieldValue("access_token", "stale.jwt").
			FieldValue("exp", strconv.FormatInt(staleExp, 10)).
			Build()
		if err := client.Do(context.Background(), reseed).Error(); err != nil {
			t.Fatalf("reseed failed: %v", err)
		}
		// release() clears the lock once the winner's call completes, so
		// this DEL is defensive rather than load-bearing — kept so this
		// subtest's outcome never depends on the previous one's cleanup.
		client.Do(context.Background(), client.B().Del().Key("v1:lock:refresh:"+sid).Build())
		calls.Store(0)

		// Every caller passes the SAME observedExp (staleExp) — the value
		// each of them "already knew" was stale before deciding to call
		// refresh(), exactly as Task 5's handler will: it reads the
		// session once, decides a refresh is warranted, then calls
		// refresh() with that reading's Exp. Using a shared external value
		// here, rather than letting each goroutine derive it from its own
		// internal read, is what makes "exactly one auth-bff call" a
		// property of the code rather than of scheduling luck — a goroutine
		// that starts late enough to read the ALREADY-refreshed session on
		// its own would otherwise have no way to distinguish "nothing
		// happened yet" from "I'm seeing the result of the refresh that
		// just finished", however tightly the start is synchronized.
		//
		// A start barrier, not a plain spawn loop, still narrows real start
		// times as far as the runtime allows — kept because it makes this
		// test a closer model of "many concurrent callers", not because it
		// is what makes the assertion below safe to rely on.
		start := make(chan struct{})
		var wg sync.WaitGroup
		tokens := make([]string, 20)
		errs := make([]error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				tokens[i], errs[i] = r.refresh(context.Background(), sid, staleExp)
			}(i)
		}
		close(start)
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("auth-bff called %d times, want exactly 1", got)
		}
		for i := range tokens {
			if errs[i] != nil {
				t.Errorf("caller %d: unexpected error: %v", i, errs[i])
				continue
			}
			if tokens[i] != "fresh.jwt" {
				t.Errorf("caller %d: token = %q, want fresh.jwt — a loser returned the stale pre-refresh token instead of waiting for the winner's write", i, tokens[i])
			}
		}
	})
}

// TestRefreshSkipsAuthBffWhenAlreadyFresh drives the short-circuit at the
// top of refresh() on purpose: it is the only path in the file that
// returns a token with no refresh occurring, and it is what makes the
// late-caller case in TestRefresh correct. Nothing exercised it
// deliberately before this test.
//
// Proving the lock was never TOUCHED needs a different signal than an
// EXISTS check on the lock key: release() deletes that key on the normal
// winner path too, so EXISTS reads 0 whether the short-circuit avoided the
// lock entirely or the code acquired it, called auth-bff, and released it
// — both leave nothing behind. onAcquireAttempt fires at the very start of
// acquire(), win or lose, so it is the one signal that actually
// distinguishes "never touched the lock" from "touched and cleaned up".
func TestRefreshSkipsAuthBffWhenAlreadyFresh(t *testing.T) {
	addr := startValkey(t)
	sid := "R123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	// The stored Exp is already comfortably ahead of what this caller
	// claims to have observed — as if someone else already refreshed this
	// session since this caller's own stale reading.
	seed(t, addr, sid, now+9000, now+36000)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"should-not-be-called"}`))
	}))
	defer server.Close()

	cfg := refreshConfig(addr, server.URL)
	s, err := newStore(cfg)
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	r := newRefresher(cfg, s)

	var acquireAttempts atomic.Int32
	r.onAcquireAttempt = func() { acquireAttempts.Add(1) }

	token, err := r.refresh(context.Background(), sid, now+5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "the.jwt" {
		t.Errorf("token = %q, want the.jwt (the already-fresh token seed() wrote)", token)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("auth-bff called %d times, want 0 — an already-fresh session must short-circuit before the lock or the HTTP call", got)
	}
	if got := acquireAttempts.Load(); got != 0 {
		t.Errorf("acquire attempted %d times, want 0 — the short-circuit must avoid the lock entirely, not merely release it afterward", got)
	}
}

// TestRefreshRejectsInvalidObservedExp pins the guard against the misuse
// class errInvalidObservedExp exists for: observedExp <= 0 must fail
// closed rather than being silently read as "everything is already
// fresher than this", which is what a bare data.Exp > observedExp
// comparison would otherwise do for every real session.
func TestRefreshRejectsInvalidObservedExp(t *testing.T) {
	addr := startValkey(t)
	sid := "S123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+5, now+36000) // a live, ordinary session

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

	for _, observedExp := range []int64{0, -1, -1000} {
		token, err := r.refresh(context.Background(), sid, observedExp)
		if !errors.Is(err, errInvalidObservedExp) {
			t.Errorf("observedExp=%d: err = %v, want errInvalidObservedExp", observedExp, err)
		}
		if token != "" {
			t.Errorf("observedExp=%d: token = %q, want empty", observedExp, token)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("auth-bff called %d times, want 0 — an invalid observedExp must fail before the HTTP call", got)
	}
}

// TestRefreshWinnerRechecksBeforeCallingAuthBff pins the winner-side
// re-check in refresh() — deleting that block leaves the whole suite
// green, since TestRefresh's winner is the first refresher and only ever
// sees fresh.Exp == observedExp (never >), TestRefreshSkipsAuthBffWhenAlreadyFresh
// short-circuits before ever acquiring the lock, and every other test has
// a single caller. None of them can exercise the fresh.Exp > observedExp
// branch after acquiring.
//
// There is no I/O boundary inside refresh() between winning the lock and
// the re-check load to race a goroutine against — both are Valkey calls
// executed back-to-back within one synchronous call, so no external
// scheduling could land a write in that window on any reliable schedule.
// afterAcquire is the seam that makes the window reachable deterministically:
// it runs exactly once, synchronously, at exactly that point.
func TestRefreshWinnerRechecksBeforeCallingAuthBff(t *testing.T) {
	addr := startValkey(t)
	sid := "T123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+5, now+36000)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"should-not-be-called"}`))
	}))
	defer server.Close()

	cfg := refreshConfig(addr, server.URL)
	s, err := newStore(cfg)
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	r := newRefresher(cfg, s)

	observedExp := now + 5
	r.afterAcquire = func() {
		// Simulate a concurrent caller's refresh completing in the window
		// between this caller winning the lock and re-checking freshness —
		// exactly the race the winner-side re-check exists to close.
		hset := client.B().Hset().Key("v1:session:"+sid).FieldValue().
			FieldValue("access_token", "concurrent.fresh.jwt").
			FieldValue("exp", strconv.FormatInt(now+9000, 10)).
			Build()
		if err := client.Do(context.Background(), hset).Error(); err != nil {
			t.Fatalf("injected write failed: %v", err)
		}
	}

	token, err := r.refresh(context.Background(), sid, observedExp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "concurrent.fresh.jwt" {
		t.Errorf("token = %q, want concurrent.fresh.jwt — the winner-side re-check must observe the write injected between acquire and the HTTP call", token)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("auth-bff called %d times, want 0 — the winner-side re-check must skip the HTTP call once it sees the injected write", got)
	}
}

// TestNewRefresherDerivesMaxWaitFromRefreshTimeout pins that maxWait tracks
// RefreshTimeoutMs rather than a hardcoded constant — nothing else in the
// suite would notice a revert to the old fixed 500ms, since every test
// configures RefreshTimeoutMs=2000 and none asserts on the derived value
// directly. No Valkey container needed: newStore tolerates an unreachable
// address at construction time (ForceSingleClient), so this runs as a
// plain unit test.
func TestNewRefresherDerivesMaxWaitFromRefreshTimeout(t *testing.T) {
	s, err := newStore(&pluginConfig{ValkeyAddr: "127.0.0.1:1", KeyPrefix: "v1:", ValkeyTimeoutMs: 300})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	cfg := &pluginConfig{RefreshTimeoutMs: 1234, RefreshLockTTLSeconds: 5}
	r := newRefresher(cfg, s)

	wantMin := time.Duration(cfg.RefreshTimeoutMs)*time.Millisecond + s.timeout
	if r.maxWait <= wantMin {
		t.Errorf("maxWait = %v, want strictly greater than RefreshTimeoutMs+store.timeout (%v) — it must track RefreshTimeoutMs, not a fixed constant", r.maxWait, wantMin)
	}
	if r.maxWait == 500*time.Millisecond {
		t.Errorf("maxWait = %v — looks like it reverted to the old hardcoded 500ms", r.maxWait)
	}
}

// TestRefreshRejectsEmptyStoredAccessToken pins tokenOrErr, shared by all
// three paths that return a token read from the store rather than from
// auth-bff's own response (the pre-lock short-circuit, the winner-side
// re-check, and waitForOtherRefresh). Exercised here via the pre-lock
// short-circuit, the simplest of the three to reach directly.
func TestRefreshRejectsEmptyStoredAccessToken(t *testing.T) {
	addr := startValkey(t)
	sid := "U123456789012345678901234567890123456789012"
	now := time.Now().Unix()

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("failed to connect to valkey: %v", err)
	}
	defer client.Close()

	key := "v1:session:" + sid
	hset := client.B().Hset().Key(key).FieldValue().
		FieldValue("ver", "1").
		FieldValue("sub", "user-1").
		FieldValue("kc_sid", "kc-1").
		FieldValue("access_token", ""). // corrupted/incomplete write
		FieldValue("exp", strconv.FormatInt(now+9000, 10)).
		FieldValue("abs_exp", strconv.FormatInt(now+36000, 10)).
		FieldValue("refresh_token_enc", "k1.iv.cipher").
		FieldValue("id_token_enc", "k1.iv.cipher").
		FieldValue("created_at", "0").
		Build()
	if err := client.Do(context.Background(), hset).Error(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	if err := client.Do(context.Background(), client.B().Expire().Key(key).Seconds(1800).Build()).Error(); err != nil {
		t.Fatalf("expire failed: %v", err)
	}

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

	// observedExp far below the stored (advanced) Exp, so this drives the
	// pre-lock short-circuit straight into tokenOrErr.
	token, err := r.refresh(context.Background(), sid, now+5)
	if err == nil {
		t.Fatalf("expected an error for an empty stored access token, got token %q with nil error", token)
	}
	if token != "" {
		t.Errorf("token = %q, want empty alongside the error", token)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("auth-bff called %d times, want 0", got)
	}
}

// TestRefreshFailureModes gives each subtest its own sid rather than sharing
// one. Even though release() now clears the refresh lock explicitly on both
// the success and failure path — via a compare-and-delete Lua script keyed
// on a per-acquisition token, so a caller only ever releases the lock it
// itself acquired — this test still gives each subtest its own sid, purely
// for isolation: reusing one would leave the 5xx subtest's outcome
// depending on the 401 subtest's timing rather than on its own behaviour.
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

		_, err = r.refresh(context.Background(), sid, now+5)
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

		_, err = r.refresh(context.Background(), sid, now+5)
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

		token, err := r.refresh(context.Background(), sid, now+5)
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

		// observedExp=0 here is not exercising errInvalidObservedExp: the
		// seeded abs_exp is ALSO corrupted (asInt64 zeroes it the same
		// way), so the abs_exp ceiling check above fires first and this
		// value is never reached. It mirrors what a real caller would
		// actually hold for this exact scenario — Task 5's handler reads
		// the same corrupted exp field and would derive 0 too.
		_, err = r.refresh(context.Background(), sid, 0)
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

		// observedExp=0 is irrelevant here too: data == nil short-circuits
		// before observedExp is ever looked at.
		_, err = r.refresh(context.Background(), "Q123456789012345678901234567890123456789012", 0)
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
