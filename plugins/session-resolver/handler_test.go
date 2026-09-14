package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func buildHandler(t *testing.T, cfg map[string]interface{}) (http.Handler, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		captured.Authorization = req.Header.Get("Authorization")
		captured.Cookie = req.Header.Get("Cookie")
		captured.Called = true
		w.WriteHeader(http.StatusOK)
	})

	handler, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: cfg},
		next,
	)
	if err != nil {
		t.Fatalf("failed to build handler: %v", err)
	}
	return handler, captured
}

type capturedRequest struct {
	Called        bool
	Authorization string
	Cookie        string
}

func handlerConfig(valkeyAddr string) map[string]interface{} {
	return map[string]interface{}{
		"valkey_addr":               valkeyAddr,
		"cookie_name":               "sid",
		"key_prefix":                "v1:",
		"valkey_timeout_ms":         500,
		"idle_ttl_seconds":          1800,
		"refresh_threshold_seconds": 30,
		"refresh_url":               "http://127.0.0.1:1/internal/sessions/{sid}/refresh",
		"internal_secret":           "s3cret",
		"allowed_origins":           []interface{}{"http://localhost:5173"},
		"skip_paths":                []interface{}{"/auth/*"},
	}
}

func TestHandlerInjectsBearerFromCookie(t *testing.T) {
	addr := startValkey(t)
	sid := "H123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+300, now+36000)

	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured.Authorization != "Bearer the.jwt" {
		t.Errorf("Authorization = %q, want Bearer the.jwt", captured.Authorization)
	}
	// The backend must never see the session identifier.
	if captured.Cookie != "" {
		t.Errorf("Cookie reached the backend: %q", captured.Cookie)
	}
}

func TestHandlerPassesBearerThroughUntouched(t *testing.T) {
	addr := startValkey(t)
	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer mcp-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if captured.Authorization != "Bearer mcp-token" {
		t.Errorf("Authorization = %q, want the caller's own token", captured.Authorization)
	}
}

func TestHandlerFailureModes(t *testing.T) {
	addr := startValkey(t)
	sid := "J123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+300, now+36000)

	expired := "K123456789012345678901234567890123456789012"
	seed(t, addr, expired, now+300, now-1) // abs_exp already passed

	for _, tc := range []struct {
		name       string
		build      func() (*http.Request, string)
		wantStatus int
		wantNext   bool
	}{
		{
			name: "unknown session is 401",
			build: func() (*http.Request, string) {
				r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
				r.AddCookie(&http.Cookie{Name: "sid", Value: "L123456789012345678901234567890123456789012"})
				return r, addr
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "session past its absolute expiry is 401",
			build: func() (*http.Request, string) {
				r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
				r.AddCookie(&http.Cookie{Name: "sid", Value: expired})
				return r, addr
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "cross origin mutation is 403",
			build: func() (*http.Request, string) {
				r := httptest.NewRequest(http.MethodPost, "/api/projects", nil)
				r.AddCookie(&http.Cookie{Name: "sid", Value: sid})
				r.Header.Set("Origin", "http://evil.example")
				return r, addr
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "skip path reaches the backend without a token",
			build: func() (*http.Request, string) {
				return httptest.NewRequest(http.MethodGet, "/auth/login/keycloak", nil), addr
			},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, valkeyAddr := tc.build()
			handler, captured := buildHandler(t, handlerConfig(valkeyAddr))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if captured.Called != tc.wantNext {
				t.Errorf("backend called = %v, want %v", captured.Called, tc.wantNext)
			}
		})
	}
}

func TestHandlerFailsClosedWhenValkeyIsDown(t *testing.T) {
	cfg := handlerConfig("127.0.0.1:1")
	cfg["valkey_timeout_ms"] = 100
	handler, captured := buildHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "M123456789012345678901234567890123456789012"})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if captured.Called {
		t.Error("backend was called while Valkey was down — must fail closed")
	}
}

// TestHandlerRefreshObservesLoadedExpNotAbsExpOrNow pins the hard requirement
// that the handler passes refresh() the Exp it already loaded from Valkey —
// not data.AbsExp, and not time.Now().Unix(). Both wrong values compile and
// pass refresh()'s own observedExp>0 guard, so nothing here is caught by the
// type system; each is only observable through a different runtime shape:
//
//   - time.Now().Unix() makes data.Exp > observedExp true for ANY session
//     still short of its refresh threshold's edge, which short-circuits
//     refresh() into returning the stale, about-to-expire token straight
//     from the store — a 200 with no auth-bff call and no error. The first
//     subtest below drives exactly that lone, non-concurrent path.
//   - data.AbsExp makes data.Exp > observedExp never true (Exp cannot
//     exceed AbsExp), which is invisible for a lone caller — the winner
//     still calls auth-bff correctly either way — but starves every
//     concurrent LOSER in waitForOtherRefresh, which polls for exactly that
//     comparison to flip. The second subtest below requires two concurrent
//     requests to surface it.
//
// Neither subtest is redundant with the other: deleting the correct
// observedExp argument and hardcoding either wrong value leaves exactly one
// of the two subtests red.
func TestHandlerRefreshObservesLoadedExpNotAbsExpOrNow(t *testing.T) {
	t.Run("a lone near-expiry request must call auth-bff, not silently reuse the stale token", func(t *testing.T) {
		addr := startValkey(t)
		sid := "N123456789012345678901234567890123456789012"
		now := time.Now().Unix()
		// Exp is inside the refresh threshold (30s) but AbsExp is far away.
		// If the handler passed time.Now().Unix(), data.Exp (now+10) would
		// read as already fresher than "now" and refresh() would never be
		// called at all.
		seed(t, addr, sid, now+10, now+36000)

		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh.jwt"}`))
		}))
		defer server.Close()

		cfg := handlerConfig(addr)
		cfg["refresh_url"] = server.URL + "/internal/sessions/{sid}/refresh"

		handler, captured := buildHandler(t, cfg)

		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("auth-bff called %d times, want exactly 1 — observedExp must be the loaded Exp, not time.Now().Unix() (which would short-circuit the call and silently reuse the stale token)", got)
		}
		if captured.Authorization != "Bearer fresh.jwt" {
			t.Errorf("Authorization = %q, want Bearer fresh.jwt — the stale pre-refresh token must never reach the backend", captured.Authorization)
		}
	})

	t.Run("a concurrent loser must not 503 waiting for a bound it can never cross", func(t *testing.T) {
		addr := startValkey(t)
		sid := "O123456789012345678901234567890123456789012"
		now := time.Now().Unix()
		client := seed(t, addr, sid, now+10, now+36000)

		var calls atomic.Int32
		var mockExp atomic.Int64
		mockExp.Store(now + 10)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls.Add(1)
			// A real auth-bff round trip is not instantaneous. Without this
			// delay the winner could finish before the loser even attempts
			// the lock, and the loser would never reach the wait loop this
			// subtest exists to exercise.
			time.Sleep(150 * time.Millisecond)
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

		cfg := handlerConfig(addr)
		cfg["refresh_url"] = server.URL + "/internal/sessions/{sid}/refresh"
		// Keep the loser's wait bound small (refresh_timeout_ms +
		// valkey_timeout_ms + a poll step) so a wrongly-bounded loser's 503
		// shows up in well under a second rather than the multi-second
		// default.
		cfg["refresh_timeout_ms"] = 300
		cfg["valkey_timeout_ms"] = 200

		handler, err := registerer(pluginName).registerHandlers(
			context.Background(),
			map[string]interface{}{pluginName: cfg},
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		)
		if err != nil {
			t.Fatalf("failed to build handler: %v", err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		statuses := make([]int, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
				req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
				rec := httptest.NewRecorder()
				<-start
				handler.ServeHTTP(rec, req)
				statuses[i] = rec.Code
			}(i)
		}
		close(start)
		wg.Wait()

		for i, status := range statuses {
			if status != http.StatusOK {
				t.Errorf("request %d: status = %d, want 200 — observedExp must be the loaded Exp, not AbsExp (which a loser can never cross, forcing a spurious 503)", i, status)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("auth-bff called %d times, want exactly 1", got)
		}
	})
}
