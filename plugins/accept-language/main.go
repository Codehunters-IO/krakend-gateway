package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

var pluginName = "krakend-accept-language"

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
	DefaultValue string `json:"default_value"`
}

func (r registerer) registerHandlers(_ context.Context, extra map[string]interface{}, h http.Handler) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	defaultValue := cfg.DefaultValue
	if defaultValue == "" {
		defaultValue = "es"
	}

	logger.Info("plugin loaded", "default_value", defaultValue)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.TrimSpace(req.Header.Get("Accept-Language")) == "" {
			req.Header.Set("Accept-Language", defaultValue)
		}
		h.ServeHTTP(w, req)
	}), nil
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
