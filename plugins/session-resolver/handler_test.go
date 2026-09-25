package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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

// TestHandlerRefreshFailureNeverLogsTheSid pins the constraint that a
// refresh failure must never put the sid into the logs. callAuthBff builds
// its request against a URL with the sid substituted in
// (.../sessions/{sid}/refresh); before refresh.go's fix, an unreachable
// auth-bff produced a *url.Error whose Error() embeds that exact URL, and
// registerHandlers logged it verbatim via err.Error() on the "refresh
// failed" line. This swaps the package-level logger for one writing to a
// buffer, drives a refresh against an address nothing listens on, and
// greps the captured output for the sid.
func TestHandlerRefreshFailureNeverLogsTheSid(t *testing.T) {
	addr := startValkey(t)
	sid := "P123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	// Exp is inside the refresh threshold, so decide's resolve path always
	// attempts a refresh; refresh_url points at a port nothing listens on,
	// so callAuthBff's HTTP call always fails.
	seed(t, addr, sid, now+5, now+36000)

	cfg := handlerConfig(addr)
	cfg["refresh_url"] = "http://127.0.0.1:1/internal/sessions/{sid}/refresh"
	cfg["refresh_timeout_ms"] = 200
	cfg["valkey_timeout_ms"] = 200

	var buf bytes.Buffer
	prev := logger
	logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("plugin", pluginName)
	t.Cleanup(func() { logger = prev })

	handler, captured := buildHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (unreachable auth-bff must fail closed)", rec.Code)
	}
	if captured.Called {
		t.Error("backend was called despite a refresh failure — must fail closed")
	}
	if strings.Contains(buf.String(), sid) {
		t.Errorf("log output contains the sid — a session identifier must never be logged:\n%s", buf.String())
	}
}

// TestHandlerEmptyStoredAccessTokenIsDenied pins the guard against a
// present-but-empty access_token: store.load only returns (nil, nil) for a
// nil Valkey reply, not for an empty string field, so a corrupted or
// partial write with a healthy Exp/AbsExp would otherwise sail past every
// check and get forwarded as a bare "Bearer " with no error at all. Task 4's
// tokenOrErr guards this for refresh()'s own return paths; this pins the
// same guard on the plain, no-refresh-needed path that never calls
// tokenOrErr.
func TestHandlerEmptyStoredAccessTokenIsDenied(t *testing.T) {
	addr := startValkey(t)
	sid := "S123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seedEmptyAccessToken(t, addr, sid, now+9000, now+36000) // far from the refresh threshold

	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if captured.Called {
		t.Error("backend was called with an empty access token — must fail closed")
	}
	if captured.Authorization == "Bearer " {
		t.Error("a bare \"Bearer \" reached the backend")
	}
}

func seedEmptyAccessToken(t *testing.T, addr, sid string, exp, absExp int64) {
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
		FieldValue("access_token", ""). // corrupted/incomplete write
		FieldValue("exp", strconv.FormatInt(exp, 10)).
		FieldValue("abs_exp", strconv.FormatInt(absExp, 10)).
		FieldValue("refresh_token_enc", "k1.iv.cipher").
		FieldValue("id_token_enc", "k1.iv.cipher").
		FieldValue("created_at", strconv.FormatInt(exp-300, 10)).
		Build()
	if err := client.Do(ctx, hset).Error(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	if err := client.Do(ctx, client.B().Expire().Key(key).Seconds(1800).Build()).Error(); err != nil {
		t.Fatalf("expire failed: %v", err)
	}
}

// TestActionResponseDeniesUnknownAction pins actionResponse's default
// branch: an action value none of the explicit cases recognize must be
// terminal (deny), never treated as actionResolve. Deleting the default
// branch leaves the switch's implicit zero-value return (0, "", false) in
// its place — terminal=false — which would silently route straight into
// session resolution instead of denying; that is exactly what this test
// would catch.
func TestActionResponseDeniesUnknownAction(t *testing.T) {
	status, outcome, terminal := actionResponse(action(99))
	if !terminal {
		t.Fatal("an unrecognized action must be terminal (deny), not fall through to session resolution")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if outcome != "unrecognized_action" {
		t.Errorf("outcome = %q, want unrecognized_action", outcome)
	}
}

// TestRefreshErrorClassNeverReflectsMessageText pins refreshErrorClass's
// actual protective property directly, on its own terms: it is
// message-text-blind. Reverting refreshErrorClass alone (main.go's log
// line back to err.Error()) while stripURL stays fixed does NOT reproduce
// a leak against TestHandlerRefreshFailureNeverLogsTheSid — stripURL
// already removes the sid from the error value at its source, so there is
// currently no live sid-bearing error left on this path for
// refreshErrorClass to catch. That makes refreshErrorClass's value
// undemonstrable by reverting-and-rerunning the handler test alone: it
// exists as defense against a FUTURE error producer on this path that
// forgets to sanitize, not against a leak reproducible today. This test
// pins that property independently: an error whose message carries a
// sid-shaped value nothing in refresh.go actually produces must still
// never have that value surface in refreshErrorClass's output.
func TestRefreshErrorClassNeverReflectsMessageText(t *testing.T) {
	sid := "Y823456789012345678901234567890123456789012"
	fake := fmt.Errorf("dial tcp: connecting to .../sessions/%s/refresh: connection refused", sid)

	class := refreshErrorClass(fake)

	if strings.Contains(class, sid) {
		t.Fatalf("refreshErrorClass reflected the input error's message text into its output: %q", class)
	}
	if class != "refresh_failed" {
		t.Errorf("class = %q, want refresh_failed for an error matching none of the known sentinels", class)
	}
}

// TestHandlerRefreshErrorMapping exercises the refresh()-error-to-status
// mapping (main.go's `if err != nil` block after refresh.refresh) through
// the full handler, not just refresh_test.go's direct calls to refresh().
// Deleting that whole block leaves the suite green otherwise: nothing else
// asserts on the status code auth-bff's own response produces once it's
// reached through registerHandlers.
func TestHandlerRefreshErrorMapping(t *testing.T) {
	t.Run("auth-bff 401 means the session is gone: handler returns 401 without forwarding", func(t *testing.T) {
		addr := startValkey(t)
		sid := "T323456789012345678901234567890123456789012"
		now := time.Now().Unix()
		seed(t, addr, sid, now+5, now+36000) // inside the refresh threshold

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		cfg := handlerConfig(addr)
		cfg["refresh_url"] = server.URL + "/internal/sessions/{sid}/refresh"

		handler, captured := buildHandler(t, cfg)

		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		if captured.Called {
			t.Error("backend was called despite auth-bff reporting the session gone")
		}
	})

	t.Run("auth-bff 500 is a transient failure: handler returns 503 without forwarding", func(t *testing.T) {
		addr := startValkey(t)
		sid := "U423456789012345678901234567890123456789012"
		now := time.Now().Unix()
		seed(t, addr, sid, now+5, now+36000)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		cfg := handlerConfig(addr)
		cfg["refresh_url"] = server.URL + "/internal/sessions/{sid}/refresh"

		handler, captured := buildHandler(t, cfg)

		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if captured.Called {
			t.Error("backend was called despite a transient refresh failure")
		}
	})
}

// TestHandlerPassThroughCookieHandling pins the coordinator's ruling on the
// pass-through branch: a request that carries its own bearer (or hits any
// other actionPassThrough path) must not forward the session cookie to the
// backend, EXCEPT on a skip path — auth-bff owns that cookie and reads it
// directly on /auth/session and /auth/logout.
func TestHandlerPassThroughCookieHandling(t *testing.T) {
	addr := startValkey(t)

	t.Run("a bearer-carrying request must not forward the session cookie to the backend", func(t *testing.T) {
		handler, captured := buildHandler(t, handlerConfig(addr))

		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.Header.Set("Authorization", "Bearer mcp-token")
		req.AddCookie(&http.Cookie{Name: "sid", Value: "V523456789012345678901234567890123456789012"})
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if captured.Authorization != "Bearer mcp-token" {
			t.Errorf("Authorization = %q, want the caller's own token", captured.Authorization)
		}
		if captured.Cookie != "" {
			t.Errorf("Cookie reached the backend on a bearer-carrying request: %q — a backend that can read the session cookie can impersonate the session", captured.Cookie)
		}
	})

	t.Run("a skip path must still receive the session cookie — auth-bff owns it", func(t *testing.T) {
		handler, captured := buildHandler(t, handlerConfig(addr))

		req := httptest.NewRequest(http.MethodGet, "/auth/login/keycloak", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "W623456789012345678901234567890123456789012"})
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if captured.Cookie == "" {
			t.Error("Cookie was stripped on a skip path — auth-bff reads this cookie directly on /auth/session and /auth/logout")
		}
	})
}

// TestHandlerMalformedSidDeniesWithoutTouchingValkey pins that a malformed
// session cookie is rejected by decide()'s actionUnauthorized branch itself
// — not by falling through to actionResolve and a Valkey lookup that
// happens to miss. Status code and Called cannot tell these apart: delete
// sidFormat from decide.go entirely and "too-short" falls through to
// actionResolve, hits load() against a real, reachable Valkey, misses, and
// denies 401 with Called==false — an externally identical result to the
// actionUnauthorized path. The outcome string in the "request denied" log
// line is where the two diverge: "malformed_session" (actionUnauthorized,
// never touches the store) versus "miss" (actionResolve, data == nil).
func TestHandlerMalformedSidDeniesWithoutTouchingValkey(t *testing.T) {
	addr := startValkey(t)

	var buf bytes.Buffer
	prev := logger
	logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("plugin", pluginName)
	t.Cleanup(func() { logger = prev })

	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "too-short"})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if captured.Called {
		t.Error("backend was called for a malformed session cookie")
	}
	if !strings.Contains(buf.String(), `"outcome":"malformed_session"`) {
		t.Errorf("expected outcome=malformed_session (actionUnauthorized) in the logs, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `"outcome":"miss"`) {
		t.Errorf("outcome=miss means this fell through to actionResolve and a real Valkey lookup instead of being rejected by actionUnauthorized:\n%s", buf.String())
	}
}

// TestHandlerLogsSuccessOutcome pins the success half of the outcome
// vocabulary. The deny paths were the only ones that logged, which made a
// gateway resolving zero sessions indistinguishable from one resolving all of
// them: both are silent. It also re-asserts the redaction rule on the lines
// that carry a live session — no sid, no cookie, no token.
func TestHandlerLogsSuccessOutcome(t *testing.T) {
	addr := startValkey(t)
	now := time.Now().Unix()

	authBff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"refreshed.jwt"}`))
	}))
	t.Cleanup(authBff.Close)

	for _, tc := range []struct {
		name        string
		sid         string
		exp         int64
		wantOutcome string
		wantToken   string
	}{
		// Far from the refresh threshold: resolved straight from the store.
		{"hit", "A123456789012345678901234567890123456789012", now + 9000, "hit", "the.jwt"},
		// Inside the refresh threshold: the refresh path runs and auth-bff answers.
		{"refreshed", "B123456789012345678901234567890123456789012", now + 5, "refreshed", "refreshed.jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed(t, addr, tc.sid, tc.exp, now+36000)

			cfg := handlerConfig(addr)
			cfg["refresh_url"] = authBff.URL + "/internal/sessions/{sid}/refresh"

			var buf bytes.Buffer
			prev := logger
			logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("plugin", pluginName)
			t.Cleanup(func() { logger = prev })

			handler, captured := buildHandler(t, cfg)

			req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
			req.AddCookie(&http.Cookie{Name: "sid", Value: tc.sid})
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", rec.Code, buf.String())
			}
			if captured.Authorization != "Bearer "+tc.wantToken {
				t.Fatalf("Authorization = %q, want %q", captured.Authorization, "Bearer "+tc.wantToken)
			}

			logged := buf.String()
			for _, want := range []string{
				`"msg":"session resolved"`,
				`"outcome":"` + tc.wantOutcome + `"`,
				`"sub":"user-1"`,
				`"path":"/api/projects"`,
			} {
				if !strings.Contains(logged, want) {
					t.Errorf("success log is missing %s:\n%s", want, logged)
				}
			}
			for _, forbidden := range []string{tc.sid, tc.wantToken} {
				if strings.Contains(logged, forbidden) {
					t.Errorf("log output contains %q — sids and tokens must never be logged:\n%s", forbidden, logged)
				}
			}
		})
	}
}
