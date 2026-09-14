package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var pluginName = "krakend-session-resolver"

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

// There is no valkey_pool_size: valkey-go does not pool connections the way
// go-redis does. It auto-pipelines concurrent commands over a small
// multiplexed set of TCP connections (ClientOption.PipelineMultiplex,
// default 2 -> 4 connections for a single-instance client) instead of
// checking a large logical pool in and out per call. A pool-size knob would
// have nothing to control, so it is dropped rather than kept and ignored.
type pluginConfig struct {
	ValkeyAddr            string   `json:"valkey_addr"`
	ValkeyPassword        string   `json:"valkey_password"`
	ValkeyDB              int      `json:"valkey_db"`
	ValkeyTimeoutMs       int      `json:"valkey_timeout_ms"`
	KeyPrefix             string   `json:"key_prefix"`
	CookieName            string   `json:"cookie_name"`
	IdleTTLSeconds        int      `json:"idle_ttl_seconds"`
	RefreshThresholdSecs  int      `json:"refresh_threshold_seconds"`
	RefreshURL            string   `json:"refresh_url"`
	RefreshTimeoutMs      int      `json:"refresh_timeout_ms"`
	RefreshLockTTLSeconds int      `json:"refresh_lock_ttl_seconds"`
	InternalSecret        string   `json:"internal_secret"`
	AllowedOrigins        []string `json:"allowed_origins"`
	CSRFSafeMethods       []string `json:"csrf_safe_methods"`
	SkipPaths             []string `json:"skip_paths"`
}

func parseConfig(extra map[string]interface{}) (*pluginConfig, error) {
	rawCfg, ok := extra[pluginName]
	if !ok {
		return nil, fmt.Errorf("configuration key %q not found", pluginName)
	}

	b, err := json.Marshal(rawCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	var cfg pluginConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Required. A misconfigured session plugin must stop the gateway, not fail open.
	if cfg.ValkeyAddr == "" {
		return nil, fmt.Errorf("valkey_addr is required")
	}
	if cfg.CookieName == "" {
		return nil, fmt.Errorf("cookie_name is required")
	}
	if cfg.InternalSecret == "" {
		return nil, fmt.Errorf("internal_secret is required")
	}
	if !strings.Contains(cfg.RefreshURL, "{sid}") {
		return nil, fmt.Errorf("refresh_url must contain the {sid} placeholder")
	}

	// Defaults.
	if cfg.ValkeyTimeoutMs == 0 {
		cfg.ValkeyTimeoutMs = 200
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "v1:"
	}
	if cfg.IdleTTLSeconds == 0 {
		cfg.IdleTTLSeconds = 1800
	}
	if cfg.RefreshThresholdSecs == 0 {
		cfg.RefreshThresholdSecs = 30
	}
	if cfg.RefreshTimeoutMs == 0 {
		cfg.RefreshTimeoutMs = 3000
	}
	if cfg.RefreshLockTTLSeconds == 0 {
		cfg.RefreshLockTTLSeconds = 5
	}
	if len(cfg.CSRFSafeMethods) == 0 {
		cfg.CSRFSafeMethods = []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	}

	return &cfg, nil
}

// registerHandlers wires the plugin into the KrakenD handler chain: parse and
// validate config (Task 1), decide what a request needs (Task 2), read the
// session from Valkey (Task 3), refresh it when it is near expiry (Task 4),
// and turn the outcome into either a forwarded request carrying a Bearer
// token or a fail-closed response. Every exit that is not actionPassThrough
// or a successful resolve ends in deny(): 401 or 503, never a silent
// pass-through without credentials.
func (r registerer) registerHandlers(
	_ context.Context,
	extra map[string]interface{},
	h http.Handler,
) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	skipExact, skipRegexes, err := buildMatchers(cfg.SkipPaths)
	if err != nil {
		return nil, fmt.Errorf("[%s] invalid skip_paths: %w", pluginName, err)
	}

	sessions, err := newStore(cfg)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to connect to valkey: %w", pluginName, err)
	}
	refresh := newRefresher(cfg, sessions)
	threshold := int64(cfg.RefreshThresholdSecs)

	logger.Info("plugin loaded",
		"valkey_addr", cfg.ValkeyAddr,
		"cookie_name", cfg.CookieName,
		"skip_paths", cfg.SkipPaths,
		"allowed_origins", cfg.AllowedOrigins)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		what, sid := decide(toRequest(req, cfg.CookieName), cfg, skipExact, skipRegexes)

		if what == actionPassThrough {
			// decide() trusts a caller-supplied bearer (or a CORS preflight,
			// or the no-cookie case it defers to jwt-headers) enough to skip
			// session resolution entirely — but "we didn't need the cookie"
			// is not license to forward one: a backend that can read the
			// session cookie can impersonate the session regardless of
			// which branch let the request through. Skip paths are the one
			// exception, since auth-bff owns this cookie and reads it
			// directly on /auth/session and /auth/logout.
			if !(skipExact[req.URL.Path] || matchesAny(req.URL.Path, skipRegexes)) {
				req.Header.Del("Cookie")
			}
			h.ServeHTTP(w, req)
			return
		}
		if status, outcome, terminal := actionResponse(what); terminal {
			deny(w, req, status, outcome)
			return
		}
		// what == actionResolve from here on — actionResponse's default
		// branch denies everything else, so falling past it here means
		// exactly that.

		ctx := req.Context()

		data, err := sessions.load(ctx, sid)
		if err != nil {
			// Valkey unreachable: fail closed. Never forward without identity.
			logger.Error("session store unavailable", "path", req.URL.Path, "err", err.Error())
			deny(w, req, http.StatusServiceUnavailable, "store_error")
			return
		}
		if data == nil {
			deny(w, req, http.StatusUnauthorized, "miss")
			return
		}
		if data.AccessToken == "" {
			// A present-but-empty access_token (a corrupted or partial
			// write) would otherwise sail past every check below — Exp/AbsExp
			// can both be healthy — and get forwarded as a bare "Bearer ".
			// Task 4's tokenOrErr guards exactly this for refresh()'s own
			// return paths; nothing guarded the plain, no-refresh-needed
			// path until now.
			deny(w, req, http.StatusUnauthorized, "miss")
			return
		}

		now := time.Now().Unix()

		if now >= data.AbsExp {
			sessions.drop(ctx, sid)
			logger.Info("session past absolute expiry", "sub", data.Subject, "outcome", "expired")
			deny(w, req, http.StatusUnauthorized, "expired")
			return
		}

		token := data.AccessToken
		if now >= data.Exp-threshold {
			// observedExp MUST be the Exp this handler just loaded above, not
			// AbsExp and not time.Now().Unix() — refresh() uses it as the
			// external, shared baseline every concurrent caller for this same
			// staleness event holds identically. See refresh.go's doc comment
			// on refresh() for what each wrong value silently breaks.
			token, err = refresh.refresh(ctx, sid, data.Exp)
			if err != nil {
				if errors.Is(err, errSessionGone) {
					deny(w, req, http.StatusUnauthorized, "refresh_session_gone")
					return
				}
				// refreshErrorClass, not err.Error(): callAuthBff's HTTP call
				// is built against a sid-parameterised URL, and an unwrapped
				// transport error's message embeds that URL. refresh.go
				// already strips it before returning, but this log line must
				// not depend on that alone for a value this sensitive.
				logger.Error("refresh failed", "sub", data.Subject, "class", refreshErrorClass(err))
				deny(w, req, http.StatusServiceUnavailable, "refresh_error")
				return
			}
		}

		sessions.renewIdle(ctx, sid)

		// Set, not Add: overwrite anything the caller smuggled in.
		req.Header.Set("Authorization", "Bearer "+token)
		// The session identifier is a credential; backends must never receive it.
		req.Header.Del("Cookie")

		h.ServeHTTP(w, req)
	}), nil
}

// actionResponse maps every decide() outcome that terminates the request
// before session resolution to a status/outcome pair. actionResolve is not
// terminal — it is the one outcome that falls through to session resolution
// — and actionPassThrough has no entry here at all, since it forwards
// rather than denies and is handled by its own branch above. Any action
// value this switch does not recognize denies via the default branch: a
// future action added to decide.go without updating this function must
// still fail closed here rather than silently entering session resolution.
func actionResponse(what action) (status int, outcome string, terminal bool) {
	switch what {
	case actionUnauthorized:
		return http.StatusUnauthorized, "malformed_session", true
	case actionForbidden:
		return http.StatusForbidden, "csrf_reject", true
	case actionResolve:
		return 0, "", false
	default:
		return http.StatusUnauthorized, "unrecognized_action", true
	}
}

// refreshErrorClass reduces a refresh() failure to a short, log-safe
// classification instead of its free-form Error() text. refresh()'s
// underlying failures can originate from an HTTP call built against a
// sid-parameterised URL (auth-bff's refresh endpoint); refresh.go strips
// that URL before returning, but the logging call site must not be the only
// thing standing between a session identifier and structured logs.
func refreshErrorClass(err error) string {
	switch {
	case errors.Is(err, errInvalidObservedExp):
		return "invalid_observed_exp"
	case errors.Is(err, errAuthBffEmptyToken):
		return "auth_bff_empty_token"
	case errors.Is(err, errNoAccessToken):
		return "empty_stored_token"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "refresh_failed"
}

func toRequest(req *http.Request, cookieName string) request {
	value := ""
	if c, err := req.Cookie(cookieName); err == nil {
		value = c.Value
	}
	return request{
		Path:          req.URL.Path,
		Method:        req.Method,
		Authorization: req.Header.Get("Authorization"),
		Origin:        req.Header.Get("Origin"),
		Referer:       req.Header.Get("Referer"),
		Cookie:        value,
	}
}

// deny writes a generic body. Never leak why authentication failed, and never
// log the sid or the cookie value.
func deny(w http.ResponseWriter, req *http.Request, status int, outcome string) {
	logger.Info("request denied", "path", req.URL.Path, "method", req.Method,
		"status", status, "outcome", outcome)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
}

func main() {}
