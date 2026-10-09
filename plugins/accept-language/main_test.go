package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// forwarded is what the backend received; the plugin's only effect is on the
// request it passes along.
func forwarded(t *testing.T, cfg map[string]interface{}, inbound ...string) string {
	t.Helper()

	var got string
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		called = true
		got = req.Header.Get("Accept-Language")
	})

	extra := map[string]interface{}{}
	if cfg != nil {
		extra[pluginName] = cfg
	}
	h, err := registerer(pluginName).registerHandlers(context.Background(), extra, next)
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	for _, v := range inbound {
		req.Header.Add("Accept-Language", v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Fatal("the backend was never reached")
	}
	return got
}

func TestAClientLanguageIsLeftAlone(t *testing.T) {
	if got := forwarded(t, nil, "en-GB"); got != "en-GB" {
		t.Fatalf("the client value was changed: %q", got)
	}
}

// A full quality-weighted header must survive intact — reducing it to the
// first tag would throw away the client's ordering.
func TestAWeightedHeaderSurvivesIntact(t *testing.T) {
	header := "fr-CH, fr;q=0.9, en;q=0.8, de;q=0.7, *;q=0.5"

	if got := forwarded(t, nil, header); got != header {
		t.Fatalf("header was rewritten: %q", got)
	}
}

func TestTheDefaultIsAppliedWhenTheHeaderIsAbsent(t *testing.T) {
	if got := forwarded(t, nil); got != "es" {
		t.Fatalf("expected the es default, got %q", got)
	}
}

// An empty or whitespace-only header is the same as no header: it carries no
// language, so the backend would have to invent one.
func TestBlankHeadersAreTreatedAsAbsent(t *testing.T) {
	for name, blank := range map[string]string{
		"empty string":   "",
		"a single space": " ",
		"spaces":         "   ",
		"a tab":          "\t",
	} {
		t.Run(name, func(t *testing.T) {
			if got := forwarded(t, nil, blank); got != "es" {
				t.Fatalf("expected the default for a %s, got %q", name, got)
			}
		})
	}
}

func TestAConfiguredDefaultReplacesEs(t *testing.T) {
	if got := forwarded(t, map[string]interface{}{"default_value": "en-US"}); got != "en-US" {
		t.Fatalf("expected en-US, got %q", got)
	}
}

func TestAnEmptyConfiguredDefaultFallsBackToEs(t *testing.T) {
	if got := forwarded(t, map[string]interface{}{"default_value": ""}); got != "es" {
		t.Fatalf("expected the es fallback, got %q", got)
	}
}

// The default must never displace a client value, whatever it is configured to.
func TestTheDefaultNeverOverridesAClientValue(t *testing.T) {
	cfg := map[string]interface{}{"default_value": "en-US"}

	if got := forwarded(t, cfg, "pt-BR"); got != "pt-BR" {
		t.Fatalf("the configured default displaced the client value: %q", got)
	}
}

// Two Accept-Language headers are not merged into one by the plugin; Set would
// collapse them, so this asserts the first value reaches the backend unaltered
// rather than being rewritten.
func TestRepeatedHeadersAreNotRewritten(t *testing.T) {
	if got := forwarded(t, nil, "en", "de"); got != "en" {
		t.Fatalf("expected the first value untouched, got %q", got)
	}
}

func TestAnUnparseableConfigFailsTheLoad(t *testing.T) {
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

func TestAConfigOfTheWrongTypeFailsTheLoad(t *testing.T) {
	_, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: map[string]interface{}{"default_value": 42}},
		http.NotFoundHandler(),
	)
	if err == nil {
		t.Fatal("expected a numeric default_value to fail the load")
	}
}

func TestParseConfigReturnsEmptyWhenTheBlockIsAbsent(t *testing.T) {
	cfg, err := parseConfig(map[string]interface{}{"some-other-plugin": map[string]interface{}{}})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.DefaultValue != "" {
		t.Fatalf("expected an empty default, got %q", cfg.DefaultValue)
	}
}

// KrakenD finds the plugin by this name; a rename breaks loading with nothing
// but a missing-plugin log line.
func TestHandlerRegistersUnderTheExpectedName(t *testing.T) {
	var registered string
	HandlerRegisterer.RegisterHandlers(func(name string, _ func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error)) {
		registered = name
	})

	if registered != "krakend-accept-language" {
		t.Fatalf("plugin registered as %q", registered)
	}
}

// Nothing about the response is this plugin's business.
func TestTheResponseIsUntouched(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h, err := registerer(pluginName).registerHandlers(context.Background(), nil, next)
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("the status was altered: %d", rec.Code)
	}
	if v := rec.Header().Get("Accept-Language"); v != "" {
		t.Fatalf("the plugin set Accept-Language on the response: %q", v)
	}
}
