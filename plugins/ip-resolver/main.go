package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var pluginName = "krakend-ip-resolver"

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
	APIBaseURL string `json:"api_base_url"`
	CacheTTL   int    `json:"cache_ttl_minutes"`
	SkipPaths  []string `json:"skip_paths"`
}

type ipInfo struct {
	Country string  `json:"country"`
	City    string  `json:"city"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Query   string  `json:"query"`
	Status  string  `json:"status"`
}

type cacheEntry struct {
	info      *ipInfo
	expiresAt time.Time
}

func (r registerer) registerHandlers(_ context.Context, extra map[string]interface{}, h http.Handler) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	skipExact := make(map[string]bool, len(cfg.SkipPaths))
	skipPrefixes := make([]string, 0)
	for _, p := range cfg.SkipPaths {
		if strings.HasSuffix(p, "/*") {
			skipPrefixes = append(skipPrefixes, strings.TrimSuffix(p, "*"))
			continue
		}
		skipExact[p] = true
	}

	var cacheTTL time.Duration
	if cfg.CacheTTL <= 0 {
		cacheTTL = 10 * time.Minute
	} else {
		cacheTTL = time.Duration(cfg.CacheTTL) * time.Minute
	}

	var (
		cache   = make(map[string]*cacheEntry)
		cacheMu sync.RWMutex
	)

	// Periodically clean up expired cache entries to avoid unbounded memory growth.
	cleanupInterval := cacheTTL
	if cleanupInterval < time.Minute {
		cleanupInterval = time.Minute
	}
	if cleanupInterval > 5*time.Minute {
		cleanupInterval = 5 * time.Minute
	}

	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()

			cacheMu.Lock()
			for key, entry := range cache {
				if entry == nil || entry.expiresAt.Before(now) {
					delete(cache, key)
				}
			}
			cacheMu.Unlock()
		}
	}()
	client := &http.Client{Timeout: 2 * time.Second}

	logger.Info("plugin loaded", "api_base_url", cfg.APIBaseURL, "cache_ttl", cacheTTL.String())

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Clear any incoming x-geo-* headers to prevent client spoofing.
		req.Header.Del("x-geo-country")
		req.Header.Del("x-geo-city")
		req.Header.Del("x-geo-latitude")
		req.Header.Del("x-geo-longitude")
		req.Header.Del("x-geo-ip")

		if skipExact[req.URL.Path] || matchesPrefix(req.URL.Path, skipPrefixes) {
			h.ServeHTTP(w, req)
			return
		}

		clientIP := extractClientIP(req)
		if clientIP == "" {
			h.ServeHTTP(w, req)
			return
		}

		info := resolve(client, cfg.APIBaseURL, clientIP, cache, &cacheMu, cacheTTL)
		if info != nil {
			req.Header.Set("x-geo-country", info.Country)
			req.Header.Set("x-geo-city", info.City)
			req.Header.Set("x-geo-latitude", fmt.Sprintf("%f", info.Lat))
			req.Header.Set("x-geo-longitude", fmt.Sprintf("%f", info.Lon))
			req.Header.Set("x-geo-ip", info.Query)
		}

		h.ServeHTTP(w, req)
	}), nil
}

// matchesPrefix returns true if path starts with any of the given prefixes.
func matchesPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func resolve(client *http.Client, baseURL, ip string, cache map[string]*cacheEntry, mu *sync.RWMutex, ttl time.Duration) *ipInfo {
	mu.RLock()
	if entry, ok := cache[ip]; ok && time.Now().Before(entry.expiresAt) {
		mu.RUnlock()
		return entry.info
	}
	mu.RUnlock()

	info := fetchIPInfo(client, baseURL, ip)
	if info == nil {
		return nil
	}

	mu.Lock()
	cache[ip] = &cacheEntry{info: info, expiresAt: time.Now().Add(ttl)}
	mu.Unlock()

	return info
}

func fetchIPInfo(client *http.Client, baseURL, ip string) *ipInfo {
	base, err := url.Parse(baseURL)
	if err != nil {
		logger.Error("invalid api_base_url", "err", err.Error())
		return nil
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/json/" + ip
	q := base.Query()
	q.Set("fields", "status,country,city,lat,lon,query")
	base.RawQuery = q.Encode()
	lookupURL := base.String()

	resp, err := client.Get(lookupURL)
	if err != nil {
		logger.Warn("ip-api call failed", "err", err.Error(), "ip", ip)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Warn("ip-api non-OK status", "status", resp.StatusCode, "ip", ip)
		return nil
	}

	var info ipInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		logger.Warn("ip-api decode failed", "err", err.Error(), "ip", ip)
		return nil
	}

	if info.Status != "success" {
		logger.Warn("ip-api lookup failed", "ip", ip, "status", info.Status)
		return nil
	}

	return &info
}

func extractClientIP(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		raw := xff
		if idx := strings.Index(xff, ","); idx != -1 {
			raw = xff[:idx]
		}
		if ip := parsePublicIP(strings.TrimSpace(raw)); ip != "" {
			return ip
		}
	}

	if xRealIP := req.Header.Get("X-Real-Ip"); xRealIP != "" {
		if ip := parsePublicIP(strings.TrimSpace(xRealIP)); ip != "" {
			return ip
		}
	}

	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	return parsePublicIP(host)
}

// privateRanges holds CIDR blocks that are private, loopback, link-local, or
// otherwise non-routable and should not be sent to the geo-IP service.
var privateRanges = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"169.254.0.0/16",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			logger.Error("failed to parse private CIDR", "cidr", cidr, "err", err.Error())
			continue
		}
		nets = append(nets, n)
	}
	return nets
}()

// parsePublicIP parses the given string as an IP address and returns its
// string representation if it is a publicly routable address; otherwise it
// returns an empty string.
func parsePublicIP(raw string) string {
	// Strip port if present (IPv6 addresses in brackets, e.g. [::1]:1234).
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	parsed := net.ParseIP(host)
	if parsed == nil {
		return ""
	}
	for _, r := range privateRanges {
		if r.Contains(parsed) {
			return ""
		}
	}
	return parsed.String()
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

	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://ip-api.com"
	}

	return &cfg, nil
}

func main() {}
