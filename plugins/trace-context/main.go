package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
)

var pluginName = "krakend-trace-context"

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

// traceparentRegex validates the W3C Trace Context traceparent header format:
// version(2)-trace_id(32)-parent_id(16)-trace_flags(2)
var traceparentRegex = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$`)

// traceIDRegex validates a bare 32-hex trace-id (fallback for clients that send only Trace-Id).
var traceIDRegex = regexp.MustCompile(`^[0-9a-f]{32}$`)

const (
	sourcePropagated  = "propagated"
	sourceFromTraceID = "from-trace-id"
	sourceGenerated   = "generated"
)

func (r registerer) registerHandlers(_ context.Context, _ map[string]interface{}, h http.Handler) (http.Handler, error) {
	logger.Info("plugin loaded", "feature", "w3c-trace-context-propagation")

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		traceparent := req.Header.Get("Traceparent")
		source := sourcePropagated

		if !traceparentRegex.MatchString(traceparent) {
			traceID := req.Header.Get("Trace-Id")
			if traceIDRegex.MatchString(traceID) {
				source = sourceFromTraceID
			} else {
				traceID = generateHexBytes(16)
				source = sourceGenerated
			}
			parentID := generateHexBytes(8)
			traceparent = fmt.Sprintf("00-%s-%s-01", traceID, parentID)
			req.Header.Set("Traceparent", traceparent)
		}

		req.Header.Set("X-Traceparent", traceparent)

		// Positions 3..35 of "00-<32hex traceId>-<16hex spanId>-01".
		traceID := traceparent[3:35]
		logger.Info("trace resolved",
			"traceId", traceID,
			"method", req.Method,
			"path", req.URL.Path,
			"source", source,
			"client_ip", req.RemoteAddr,
		)

		h.ServeHTTP(w, req)
	}), nil
}

// generateHexBytes generates n random bytes and returns their hex encoding.
func generateHexBytes(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		logger.Error("rand failed", "err", err.Error())
		// Fallback: return zeros.
		return hex.EncodeToString(make([]byte, n))
	}
	return hex.EncodeToString(b)
}

func main() {}
