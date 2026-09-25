package main

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	valid := map[string]interface{}{
		pluginName: map[string]interface{}{
			"valkey_addr":               "valkey:6379",
			"cookie_name":               "sid",
			"key_prefix":                "v1:",
			"idle_ttl_seconds":          1800,
			"refresh_threshold_seconds": 30,
			"refresh_url":               "http://auth-bff:8086/internal/sessions/{sid}/refresh",
			"internal_secret":           "s3cret",
			"allowed_origins":           []interface{}{"http://localhost:5173"},
			"skip_paths":                []interface{}{"/auth/*"},
		},
	}

	t.Run("parses a complete config", func(t *testing.T) {
		cfg, err := parseConfig(valid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ValkeyAddr != "valkey:6379" {
			t.Errorf("valkey_addr = %q, want valkey:6379", cfg.ValkeyAddr)
		}
		if cfg.CookieName != "sid" {
			t.Errorf("cookie_name = %q, want sid", cfg.CookieName)
		}
		if len(cfg.AllowedOrigins) != 1 {
			t.Errorf("allowed_origins length = %d, want 1", len(cfg.AllowedOrigins))
		}
	})

	t.Run("applies defaults for optional fields", func(t *testing.T) {
		cfg, _ := parseConfig(valid)
		if cfg.ValkeyTimeoutMs != 200 {
			t.Errorf("valkey_timeout_ms default = %d, want 200", cfg.ValkeyTimeoutMs)
		}
		if cfg.RefreshTimeoutMs != 3000 {
			t.Errorf("refresh_timeout_ms default = %d, want 3000", cfg.RefreshTimeoutMs)
		}
		if cfg.RefreshLockTTLSeconds != 5 {
			t.Errorf("refresh_lock_ttl_seconds default = %d, want 5", cfg.RefreshLockTTLSeconds)
		}
		if len(cfg.CSRFSafeMethods) != 3 {
			t.Errorf("csrf_safe_methods default = %v, want GET/HEAD/OPTIONS", cfg.CSRFSafeMethods)
		}
	})

	// key_prefix is the cross-repo key namespace: auth-bff writes v1:session:{sid}
	// and this plugin must read the same keys. Every other default is local to
	// the plugin; getting this one wrong makes it read an empty namespace and
	// 401 every cookie-carrying request, so the default is pinned here rather
	// than only implied by the settings file that happens to set it today.
	t.Run("defaults key_prefix to the v1: namespace auth-bff writes", func(t *testing.T) {
		m := cloneConfigMap(valid)
		delete(m[pluginName].(map[string]interface{}), "key_prefix")

		cfg, err := parseConfig(m)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.KeyPrefix != "v1:" {
			t.Errorf("key_prefix default = %q, want v1:", cfg.KeyPrefix)
		}
	})

	// Misconfiguration must stop the gateway from starting, never degrade silently.
	for _, tc := range []struct {
		name    string
		mutate  func(m map[string]interface{})
		wantErr string
	}{
		{"missing config key", func(m map[string]interface{}) { delete(m, pluginName) }, "not found"},
		{"empty valkey_addr", func(m map[string]interface{}) { m[pluginName].(map[string]interface{})["valkey_addr"] = "" }, "valkey_addr"},
		{"empty cookie_name", func(m map[string]interface{}) { m[pluginName].(map[string]interface{})["cookie_name"] = "" }, "cookie_name"},
		{"empty internal_secret", func(m map[string]interface{}) { m[pluginName].(map[string]interface{})["internal_secret"] = "" }, "internal_secret"},
		{"refresh_url without sid placeholder", func(m map[string]interface{}) {
			m[pluginName].(map[string]interface{})["refresh_url"] = "http://auth-bff:8086/refresh"
		}, "{sid}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloneConfigMap(valid)
			tc.mutate(cfg)
			_, err := parseConfig(cfg)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func cloneConfigMap(src map[string]interface{}) map[string]interface{} {
	inner, ok := src[pluginName].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	clonedInner := make(map[string]interface{}, len(inner))
	for k, v := range inner {
		clonedInner[k] = v
	}
	return map[string]interface{}{pluginName: clonedInner}
}
