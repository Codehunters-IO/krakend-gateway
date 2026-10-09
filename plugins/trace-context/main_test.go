package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

const (
	validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	validTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
)

var generatedShape = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)

// seen is what the backend received, which is the only thing that matters:
// this plugin's whole output is the request it forwards.
type seen struct {
	called       bool
	traceparent  string
	xTraceparent string
	traceID      string
}

func buildHandler(t *testing.T) (http.Handler, *seen) {
	t.Helper()
	got := &seen{}
	next := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		got.called = true
		got.traceparent = req.Header.Get("Traceparent")
		got.xTraceparent = req.Header.Get("X-Traceparent")
		got.traceID = req.Header.Get("Trace-Id")
	})

	h, err := registerer(pluginName).registerHandlers(context.Background(), nil, next)
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}
	return h, got
}

// do sends one request with the given headers and returns what the backend saw.
func do(t *testing.T, headers map[string]string) (*seen, *httptest.ResponseRecorder) {
	t.Helper()
	h, got := buildHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !got.called {
		t.Fatal("the backend was never reached")
	}
	return got, rec
}

func TestAValidTraceparentIsPropagatedUnchanged(t *testing.T) {
	got, _ := do(t, map[string]string{"Traceparent": validTraceparent})

	if got.traceparent != validTraceparent {
		t.Fatalf("traceparent was rewritten: %q", got.traceparent)
	}
	if got.xTraceparent != validTraceparent {
		t.Fatalf("X-Traceparent should mirror it, got %q", got.xTraceparent)
	}
}

// Sampling flags are the caller's decision, not ours: an unsampled trace must
// stay unsampled through the edge.
func TestInboundTraceFlagsAreNotOverridden(t *testing.T) {
	unsampled := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"

	got, _ := do(t, map[string]string{"Traceparent": unsampled})

	if got.traceparent != unsampled {
		t.Fatalf("flags were changed: %q", got.traceparent)
	}
}

// The legacy fallback: a client that only knows Trace-Id keeps its trace id,
// and the edge supplies the span.
func TestABareTraceIdIsLiftedIntoATraceparent(t *testing.T) {
	got, _ := do(t, map[string]string{"Trace-Id": validTraceID})

	if !generatedShape.MatchString(got.traceparent) {
		t.Fatalf("traceparent has the wrong shape: %q", got.traceparent)
	}
	if got.traceparent[3:35] != validTraceID {
		t.Fatalf("the inbound trace id was lost: %q", got.traceparent)
	}
}

func TestATraceparentIsGeneratedWhenNothingUsableArrives(t *testing.T) {
	got, _ := do(t, nil)

	if !generatedShape.MatchString(got.traceparent) {
		t.Fatalf("generated traceparent has the wrong shape: %q", got.traceparent)
	}
	if got.xTraceparent != got.traceparent {
		t.Fatalf("X-Traceparent %q does not mirror %q", got.xTraceparent, got.traceparent)
	}
}

// Two requests must not share a trace id, or every request collapses into one
// trace and the header is worse than useless.
func TestGeneratedTraceIdsAreNotReused(t *testing.T) {
	first, _ := do(t, nil)
	second, _ := do(t, nil)

	if first.traceparent == second.traceparent {
		t.Fatalf("two requests got the same traceparent: %q", first.traceparent)
	}
	if first.traceparent[3:35] == second.traceparent[3:35] {
		t.Fatalf("two requests got the same trace id: %q", first.traceparent[3:35])
	}
}

// Every malformed shape must fall through to generation rather than being
// propagated — a backend that trusts the header would otherwise inherit
// garbage from the client.
func TestMalformedTraceparentsAreReplaced(t *testing.T) {
	cases := map[string]string{
		"empty":                   "",
		"only whitespace":         "   ",
		"uppercase hex":           "00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01",
		"trace id too short":      "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",
		"trace id too long":       "00-4bf92f3577b34da6a3ce929d0e0e47366-00f067aa0ba902b7-01",
		"span id too short":       "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b-01",
		"missing the flags field": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
		"an extra field":          "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-99",
		"non hex characters":      "00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"underscores for dashes":  "00_4bf92f3577b34da6a3ce929d0e0e4736_00f067aa0ba902b7_01",
		"leading space":           " 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"trailing newline":        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01\n",
		"a plausible looking lie": "not-a-traceparent",
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			got, _ := do(t, map[string]string{"Traceparent": bad})

			if got.traceparent == bad {
				t.Fatalf("a malformed traceparent was propagated: %q", bad)
			}
			if !generatedShape.MatchString(got.traceparent) {
				t.Fatalf("replacement has the wrong shape: %q", got.traceparent)
			}
		})
	}
}

// Same strictness for the legacy header: anything that is not 32 lowercase hex
// is ignored and a fresh trace id is generated.
func TestMalformedTraceIdsAreIgnored(t *testing.T) {
	for name, bad := range map[string]string{
		"uppercase":   "4BF92F3577B34DA6A3CE929D0E0E4736",
		"too short":   "4bf92f3577b34da6a3ce929d0e0e473",
		"too long":    "4bf92f3577b34da6a3ce929d0e0e47366",
		"non hex":     "zzf92f3577b34da6a3ce929d0e0e4736",
		"with dashes": "4bf92f35-77b3-4da6-a3ce-929d0e0e4736",
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := do(t, map[string]string{"Trace-Id": bad})

			if got.traceparent[3:35] == bad {
				t.Fatalf("a malformed trace id was adopted: %q", bad)
			}
			if !generatedShape.MatchString(got.traceparent) {
				t.Fatalf("replacement has the wrong shape: %q", got.traceparent)
			}
		})
	}
}

// A valid traceparent wins over Trace-Id; they are not merged, and the legacy
// header must not be able to redirect a real trace.
func TestTraceparentWinsOverTraceId(t *testing.T) {
	other := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	got, _ := do(t, map[string]string{
		"Traceparent": validTraceparent,
		"Trace-Id":    other,
	})

	if got.traceparent != validTraceparent {
		t.Fatalf("Trace-Id displaced a valid traceparent: %q", got.traceparent)
	}
}

// A client-supplied X-Traceparent must never reach the backend as its own: the
// plugin overwrites it with what it resolved.
func TestClientSuppliedXTraceparentIsOverwritten(t *testing.T) {
	got, _ := do(t, map[string]string{
		"Traceparent":   validTraceparent,
		"X-Traceparent": "00-11111111111111111111111111111111-2222222222222222-01",
	})

	if got.xTraceparent != validTraceparent {
		t.Fatalf("a spoofed X-Traceparent survived: %q", got.xTraceparent)
	}
}

// The legacy header is left as the client sent it — nothing downstream should
// read it, but the plugin does not claim to normalise it either.
func TestTheLegacyTraceIdHeaderIsLeftAsReceived(t *testing.T) {
	got, _ := do(t, map[string]string{"Trace-Id": validTraceID})

	if got.traceID != validTraceID {
		t.Fatalf("Trace-Id was modified: %q", got.traceID)
	}
}

// Documented gap, asserted so a future fix is a deliberate change: the regex
// accepts an all-zero trace id, which W3C Trace Context declares invalid. The
// plugin propagates it rather than generating a real one.
func TestAnAllZeroTraceIdIsCurrentlyPropagated(t *testing.T) {
	allZero := "00-00000000000000000000000000000000-0000000000000000-01"

	got, _ := do(t, map[string]string{"Traceparent": allZero})

	if got.traceparent != allZero {
		t.Fatalf("behaviour changed — an invalid all-zero trace is no longer propagated (%q). "+
			"That is an improvement: update this test and the trace section of docs/session-flow.md",
			got.traceparent)
	}
}

// Same shape of gap: version ff is reserved and invalid, and is propagated.
func TestTheReservedFfVersionIsCurrentlyPropagated(t *testing.T) {
	reserved := "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	got, _ := do(t, map[string]string{"Traceparent": reserved})

	if got.traceparent != reserved {
		t.Fatalf("behaviour changed — version ff is no longer propagated (%q). "+
			"Update this test if that was deliberate", got.traceparent)
	}
}

// The plugin only touches the request. Nothing is added to the response, which
// is why a traceparent reaches the browser only if a backend sets it — worth
// knowing when reading the CORS expose_headers list.
func TestNothingIsAddedToTheResponse(t *testing.T) {
	_, rec := do(t, map[string]string{"Traceparent": validTraceparent})

	for _, header := range []string{"Traceparent", "X-Traceparent", "Trace-Id"} {
		if v := rec.Header().Get(header); v != "" {
			t.Fatalf("the plugin set %s: %q on the response", header, v)
		}
	}
}

func TestGenerateHexBytesReturnsTheRequestedWidth(t *testing.T) {
	if got := generateHexBytes(16); len(got) != 32 {
		t.Fatalf("expected 32 hex characters for 16 bytes, got %d (%q)", len(got), got)
	}
	if got := generateHexBytes(8); len(got) != 16 {
		t.Fatalf("expected 16 hex characters for 8 bytes, got %d (%q)", len(got), got)
	}
}

// KrakenD finds the plugin by this name; a rename breaks loading with nothing
// but a missing-plugin log line.
func TestHandlerRegistersUnderTheExpectedName(t *testing.T) {
	var registered string
	HandlerRegisterer.RegisterHandlers(func(name string, _ func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error)) {
		registered = name
	})

	if registered != "krakend-trace-context" {
		t.Fatalf("plugin registered as %q", registered)
	}
}

// The plugin takes no configuration, so an unexpected block must not stop it
// loading.
func TestLoadingIgnoresAnyConfigBlock(t *testing.T) {
	_, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: map[string]interface{}{"unexpected": true}},
		http.NotFoundHandler(),
	)
	if err != nil {
		t.Fatalf("expected the config block to be ignored, got %v", err)
	}
}
