package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

var pluginName = "krakend-gateway-timeout"

var logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
	With("plugin", pluginName)

// HandlerRegisterer is the symbol KrakenD looks for to load the plugin.
var HandlerRegisterer = registerer(pluginName)

type registerer string

func (r registerer) RegisterHandlers(f func(
	name string,
	handler func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error),
)) {
	f(string(r), r.registerHandlers)
}

type pluginConfig struct {
	TriggerStatus int    `json:"trigger_status"`
	TimeoutStatus int    `json:"timeout_status"`
	MinElapsed    string `json:"min_elapsed"`
}

// KrakenD Community returns 500 (not 504) when a backend times out. This
// server plugin wraps the response and rewrites that timeout-induced 500 into
// a 504 Gateway Timeout. A timeout can only surface after the endpoint timeout
// has elapsed, so the rewrite is gated on min_elapsed to leave genuine fast
// 500s from the backend untouched.
func (r registerer) registerHandlers(_ context.Context, extra map[string]interface{}, h http.Handler) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	triggerStatus := cfg.TriggerStatus
	if triggerStatus == 0 {
		triggerStatus = http.StatusInternalServerError
	}
	timeoutStatus := cfg.TimeoutStatus
	if timeoutStatus == 0 {
		timeoutStatus = http.StatusGatewayTimeout
	}
	minElapsed := 4900 * time.Millisecond
	if cfg.MinElapsed != "" {
		d, err := time.ParseDuration(cfg.MinElapsed)
		if err != nil {
			return nil, fmt.Errorf("[%s] invalid min_elapsed %q: %w", pluginName, cfg.MinElapsed, err)
		}
		minElapsed = d
	}

	logger.Info("plugin loaded",
		"trigger_status", triggerStatus,
		"timeout_status", timeoutStatus,
		"min_elapsed", minElapsed.String())

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rw := &timeoutRewriter{
			ResponseWriter: w,
			start:          time.Now(),
			triggerStatus:  triggerStatus,
			timeoutStatus:  timeoutStatus,
			minElapsed:     minElapsed,
			path:           req.URL.Path,
		}
		h.ServeHTTP(rw, req)
		rw.finalize()
	}), nil
}

// timeoutRewriter separates genuine backend 500s from KrakenD timeouts. A real
// backend error (no-op encoding) carries a body; a timeout yields an empty 500.
// The trigger status is therefore held back — not the whole response — so we can
// inspect body emptiness before committing. A held 500 becomes a 504 only when
// it is empty AND slow (>= minElapsed), which keeps fast infra errors (e.g.
// connection refused) as 500. Every other status streams through untouched, so
// SSE endpoints are never buffered.
type timeoutRewriter struct {
	http.ResponseWriter
	start         time.Time
	triggerStatus int
	timeoutStatus int
	minElapsed    time.Duration
	path          string

	committed    bool // status already forwarded to the real writer
	holdingError bool // trigger status seen, decision deferred to finalize
	bodyWritten  bool
}

func (t *timeoutRewriter) WriteHeader(code int) {
	if t.committed || t.holdingError {
		return
	}
	if code == t.triggerStatus {
		t.holdingError = true // defer: could be a timeout (empty body)
		return
	}
	t.committed = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *timeoutRewriter) Write(b []byte) (int, error) {
	if t.holdingError {
		if len(b) == 0 {
			return 0, nil // still undecided; wait for real bytes or finalize
		}
		// Backend produced a body → genuine 500, commit it as-is.
		t.holdingError = false
		t.committed = true
		t.bodyWritten = true
		t.ResponseWriter.WriteHeader(t.triggerStatus)
		return t.ResponseWriter.Write(b)
	}
	if !t.committed {
		t.committed = true
		t.ResponseWriter.WriteHeader(http.StatusOK)
	}
	if len(b) > 0 {
		t.bodyWritten = true
	}
	return t.ResponseWriter.Write(b)
}

// finalize resolves a held trigger status once the handler has returned.
func (t *timeoutRewriter) finalize() {
	if !t.holdingError {
		return
	}
	t.holdingError = false
	t.committed = true

	status := t.triggerStatus
	if !t.bodyWritten && time.Since(t.start) >= t.minElapsed {
		logger.Warn("empty slow backend error mapped to gateway timeout",
			"path", t.path,
			"elapsed_ms", time.Since(t.start).Milliseconds(),
			"from_status", t.triggerStatus,
			"to_status", t.timeoutStatus)
		status = t.timeoutStatus
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *timeoutRewriter) Flush() {
	// Never flush while a trigger status is held (would leak an empty 500).
	if t.holdingError {
		return
	}
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *timeoutRewriter) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}

func parseConfig(extra map[string]interface{}) (*pluginConfig, error) {
	rawCfg, ok := extra[pluginName]
	if !ok {
		return &pluginConfig{}, nil
	}

	b, err := json.Marshal(rawCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	var cfg pluginConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &cfg, nil
}

func main() {}
