package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// buildHandler wires the plugin around a backend of the test's choosing. cfg is
// the plugin's own config block, as KrakenD would hand it over.
func buildHandler(t *testing.T, cfg map[string]interface{}, backend http.HandlerFunc) http.Handler {
	t.Helper()
	extra := map[string]interface{}{}
	if cfg != nil {
		extra[pluginName] = cfg
	}
	h, err := registerer(pluginName).registerHandlers(context.Background(), extra, backend)
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}
	return h
}

func serve(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	return rec
}

// fastConfig keeps min_elapsed at zero so "slow" is true for every request,
// which is what lets these tests assert the rewrite without sleeping.
func fastConfig() map[string]interface{} {
	return map[string]interface{}{"min_elapsed": "0s"}
}

// The whole point of the plugin: KrakenD Community answers 500 when a backend
// times out, and an empty slow 500 is the only shape that means "timeout".
func TestEmptySlowTriggerStatusBecomesGatewayTimeout(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	rec := serve(h)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("expected no body, got %q", body)
	}
}

// A 500 that arrives before min_elapsed cannot be a timeout — the endpoint
// timeout has not elapsed yet. Connection refused looks exactly like this.
func TestEmptyFastTriggerStatusStaysFiveHundred(t *testing.T) {
	h := buildHandler(t, map[string]interface{}{"min_elapsed": "1h"},
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

	rec := serve(h)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a fast empty error, got %d", rec.Code)
	}
}

// A backend that produced a body answered for itself, however slow it was.
// Rewriting that to 504 would lie about where the error came from.
func TestSlowTriggerStatusWithABodyStaysFiveHundred(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"backend blew up"}`))
	})

	rec := serve(h)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if got, want := rec.Body.String(), `{"message":"backend blew up"}`; got != want {
		t.Fatalf("body was altered: got %q want %q", got, want)
	}
}

// The body must survive byte for byte across several Write calls, and the
// status must be sent exactly once.
func TestHeldErrorBodyIsForwardedOnceAcrossMultipleWrites(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("first "))
		_, _ = w.Write([]byte("second"))
	})

	spy := &spyWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(spy, httptest.NewRequest(http.MethodGet, "/api/projects", nil))

	if spy.headerCalls != 1 {
		t.Fatalf("expected exactly one WriteHeader, got %d", spy.headerCalls)
	}
	if spy.status != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", spy.status)
	}
	if got, want := spy.body.String(), "first second"; got != want {
		t.Fatalf("body got %q want %q", got, want)
	}
}

// finalize's `!t.bodyWritten` guard cannot be reached through ServeHTTP: any
// Write with content while an error is held clears holdingError on the spot,
// so finalize only ever runs with bodyWritten false. Mutation testing proved
// it — removing that guard broke no test routed through the handler. It is
// defensive code for a state the writer does not currently produce, which is
// worth keeping and worth pinning directly, or the next refactor silently
// changes what an already-bodied error resolves to.
func TestFinalizeKeepsTheErrorWhenABodyWasAlreadyWritten(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &timeoutRewriter{
		ResponseWriter: rec,
		start:          time.Now().Add(-time.Hour), // unambiguously slow
		triggerStatus:  http.StatusInternalServerError,
		timeoutStatus:  http.StatusGatewayTimeout,
		minElapsed:     time.Second,
		holdingError:   true,
		bodyWritten:    true,
	}

	rw.finalize()

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a held error that already carried a body must stay 500, got %d", rec.Code)
	}
}

// Everything that is not the trigger status streams straight through. SSE
// endpoints depend on this: a buffered 200 would hold the stream open with
// nothing reaching the client.
func TestNonTriggerStatusesPassThroughUntouched(t *testing.T) {
	for _, status := range []int{
		http.StatusOK,
		http.StatusNoContent,
		http.StatusNotFound,
		http.StatusUnauthorized,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})

		if rec := serve(h); rec.Code != status {
			t.Errorf("status %d was rewritten to %d", status, rec.Code)
		}
	}
}

// 503 deserves its own assertion: it is the status both auth plugins return
// when their dependency is down, and turning it into 504 would misattribute a
// JWKS or Valkey outage to the backend.
func TestServiceUnavailableIsNeverRewritten(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"session store unavailable"}`))
	})

	rec := serve(h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 to survive, got %d", rec.Code)
	}
}

// A handler that writes a body without calling WriteHeader means 200, the same
// as it does for a bare http.ResponseWriter.
func TestWriteWithoutWriteHeaderImpliesOK(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	rec := serve(h)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected an implicit 200, got %d", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body got %q", rec.Body.String())
	}
}

// A handler that writes nothing at all must not provoke a header of its own:
// finalize has no held status to resolve.
func TestSilentBackendProducesNoHeaderOfOurOwn(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(http.ResponseWriter, *http.Request) {})

	spy := &spyWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(spy, httptest.NewRequest(http.MethodGet, "/api/projects", nil))

	if spy.headerCalls != 0 {
		t.Fatalf("expected no WriteHeader call, got %d (status %d)", spy.headerCalls, spy.status)
	}
}

// A second WriteHeader is a bug in the backend, not an instruction. Forwarding
// it would log "superfluous WriteHeader" at best and corrupt the status at
// worst.
func TestSecondWriteHeaderIsIgnored(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.WriteHeader(http.StatusOK)
	})

	spy := &spyWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(spy, httptest.NewRequest(http.MethodGet, "/api/projects", nil))

	if spy.headerCalls != 1 {
		t.Fatalf("expected one WriteHeader, got %d", spy.headerCalls)
	}
	if spy.status != http.StatusNotFound {
		t.Fatalf("expected the first status to win, got %d", spy.status)
	}
}

// A trigger status followed by a second WriteHeader must stay held, so the
// empty-and-slow decision is still the one that resolves it.
func TestTriggerStatusFollowedByAnotherStatusStaysHeld(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.WriteHeader(http.StatusOK)
	})

	if rec := serve(h); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected the held 500 to resolve to 504, got %d", rec.Code)
	}
}

// Flush must not escape while a trigger status is held: flushing would commit
// an empty 500 that the plugin exists to rewrite.
func TestFlushIsSuppressedWhileAnErrorIsHeld(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.(http.Flusher).Flush()
	})

	spy := &spyWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(spy, httptest.NewRequest(http.MethodGet, "/api/projects", nil))

	if spy.flushes != 0 {
		t.Fatalf("expected the flush to be swallowed, got %d", spy.flushes)
	}
	if spy.status != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", spy.status)
	}
}

// And it must reach the real writer on the streaming path, or SSE buffers.
func TestFlushReachesTheWriterOnTheStreamingPath(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: tick\n\n"))
		w.(http.Flusher).Flush()
	})

	spy := &spyWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(spy, httptest.NewRequest(http.MethodGet, "/api/projects/1/refinement/stream", nil))

	if spy.flushes != 1 {
		t.Fatalf("expected one flush to pass through, got %d", spy.flushes)
	}
}

// Headers the backend set are the real writer's, not a copy, so they survive
// whatever the rewrite decides.
func TestBackendHeadersSurviveTheRewrite(t *testing.T) {
	h := buildHandler(t, fastConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "abc123")
		w.WriteHeader(http.StatusInternalServerError)
	})

	rec := serve(h)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "abc123" {
		t.Fatalf("header lost: got %q", got)
	}
}

// min_elapsed is the whole discriminator between "infra failed fast" and
// "backend timed out", so a real wait is worth one short test.
func TestMinElapsedIsMeasuredNotAssumed(t *testing.T) {
	h := buildHandler(t, map[string]interface{}{"min_elapsed": "40ms"},
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(60 * time.Millisecond)
			w.WriteHeader(http.StatusInternalServerError)
		})

	if rec := serve(h); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 once min_elapsed passed, got %d", rec.Code)
	}

	h = buildHandler(t, map[string]interface{}{"min_elapsed": "500ms"},
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

	if rec := serve(h); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 below min_elapsed, got %d", rec.Code)
	}
}

// Both statuses are configurable, and nothing may hardcode 500/504 behind the
// config.
func TestTriggerAndTimeoutStatusesAreHonoured(t *testing.T) {
	cfg := map[string]interface{}{
		"trigger_status": 502,
		"timeout_status": 599,
		"min_elapsed":    "0s",
	}

	h := buildHandler(t, cfg, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if rec := serve(h); rec.Code != 599 {
		t.Fatalf("expected the configured 599, got %d", rec.Code)
	}

	// With 502 as the trigger, a plain 500 is now an ordinary status.
	h = buildHandler(t, cfg, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if rec := serve(h); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 to pass through untouched, got %d", rec.Code)
	}
}

func TestDefaultsApplyWhenTheConfigBlockIsAbsent(t *testing.T) {
	// No config block at all: trigger 500, timeout 504, min_elapsed 4900ms.
	h := buildHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	// Well under the 4900ms default, so the 500 must stand.
	if rec := serve(h); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected the default min_elapsed to protect a fast 500, got %d", rec.Code)
	}
}

func TestZeroValuedStatusesFallBackToTheDefaults(t *testing.T) {
	h := buildHandler(t, map[string]interface{}{
		"trigger_status": 0,
		"timeout_status": 0,
		"min_elapsed":    "0s",
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if rec := serve(h); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 500→504 from the zero-value defaults, got %d", rec.Code)
	}
}

func TestInvalidMinElapsedFailsTheLoadInsteadOfGuessing(t *testing.T) {
	_, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: map[string]interface{}{"min_elapsed": "nope"}},
		http.NotFoundHandler(),
	)
	if err == nil {
		t.Fatal("expected an invalid min_elapsed to fail the load")
	}
}

func TestUnparseableConfigFailsTheLoad(t *testing.T) {
	// A channel cannot be marshalled to JSON, which is how parseConfig
	// discovers a config shape it cannot read.
	_, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: make(chan int)},
		http.NotFoundHandler(),
	)
	if err == nil {
		t.Fatal("expected an unmarshallable config to fail the load")
	}
}

func TestConfigOfTheWrongTypeFailsTheLoad(t *testing.T) {
	_, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: map[string]interface{}{"trigger_status": "five hundred"}},
		http.NotFoundHandler(),
	)
	if err == nil {
		t.Fatal("expected a string trigger_status to fail the load")
	}
}

// KrakenD finds the plugin by this symbol and name; a rename breaks loading
// with nothing but a missing-plugin log line.
func TestHandlerRegistersUnderTheExpectedName(t *testing.T) {
	var registered string
	HandlerRegisterer.RegisterHandlers(func(name string, _ func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error)) {
		registered = name
	})

	if registered != "krakend-gateway-timeout" {
		t.Fatalf("plugin registered as %q", registered)
	}
}

// Unwrap is what lets the stdlib and any outer wrapper reach the real writer.
func TestUnwrapExposesTheUnderlyingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &timeoutRewriter{ResponseWriter: rec}

	if rw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap did not return the wrapped writer")
	}
}

// Documented gap, asserted so it is a decision and not a surprise: the wrapper
// does not implement http.Hijacker. Anything needing to take over the
// connection — a websocket upgrade — cannot do it through this plugin.
func TestWrapperDoesNotImplementHijacker(t *testing.T) {
	var w http.ResponseWriter = &timeoutRewriter{ResponseWriter: httptest.NewRecorder()}

	if _, ok := w.(http.Hijacker); ok {
		t.Fatal("the wrapper now implements Hijacker; update this test and docs/session-flow.md")
	}
}

// spyWriter counts what reached the real writer, which a ResponseRecorder
// cannot show: how many times the status was sent, and whether Flush escaped.
type spyWriter struct {
	http.ResponseWriter
	status      int
	headerCalls int
	flushes     int
	body        bodyBuffer
}

type bodyBuffer []byte

func (b *bodyBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

func (b bodyBuffer) String() string { return string(b) }

func (s *spyWriter) WriteHeader(code int) {
	s.headerCalls++
	if s.headerCalls == 1 {
		s.status = code
	}
}

func (s *spyWriter) Write(p []byte) (int, error) { return s.body.Write(p) }

func (s *spyWriter) Flush() { s.flushes++ }
