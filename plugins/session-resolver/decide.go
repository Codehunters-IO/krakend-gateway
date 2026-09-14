package main

import (
	"net/url"
	"regexp"
	"strings"
)

type action int

const (
	actionPassThrough action = iota // bearer present, skip path, or preflight
	actionUnauthorized
	actionForbidden
	actionResolve // read Valkey and inject
)

type request struct {
	Path          string
	Method        string
	Authorization string
	Origin        string
	Referer       string
	Cookie        string
}

var sidFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// decide is the whole policy of the plugin, expressed without I/O.
func decide(
	req request,
	cfg *pluginConfig,
	skipExact map[string]bool,
	skipRegexes []*regexp.Regexp,
) (action, string) {
	if skipExact[req.Path] || matchesAny(req.Path, skipRegexes) {
		return actionPassThrough, ""
	}

	// CORS preflights carry no credentials and must never be challenged.
	if req.Method == "OPTIONS" {
		return actionPassThrough, ""
	}

	// A caller-supplied bearer always wins: this is what keeps MCP, CI and mobile
	// working. jwt-headers still validates it, so trusting it costs nothing.
	if req.Authorization != "" {
		return actionPassThrough, ""
	}

	// No cookie either: let jwt-headers issue the 401 with its own message.
	if req.Cookie == "" {
		return actionPassThrough, ""
	}

	if !sidFormat.MatchString(req.Cookie) {
		return actionUnauthorized, ""
	}

	if !isSafeMethod(req.Method, cfg.CSRFSafeMethods) && !originAllowed(req, cfg.AllowedOrigins) {
		return actionForbidden, ""
	}

	return actionResolve, req.Cookie
}

func isSafeMethod(method string, safe []string) bool {
	for _, m := range safe {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// originAllowed checks Origin, falling back to Referer's scheme+host. A mutating
// request with neither is rejected: under SameSite=Lax the cookie can still reach
// us cross-site, and an absent Origin is exactly what a forged request looks like.
func originAllowed(req request, allowed []string) bool {
	candidate := req.Origin
	if candidate == "" && req.Referer != "" {
		if u, err := url.Parse(req.Referer); err == nil && u.Scheme != "" && u.Host != "" {
			candidate = u.Scheme + "://" + u.Host
		}
	}
	if candidate == "" {
		return false
	}
	for _, a := range allowed {
		if a == candidate {
			return true
		}
	}
	return false
}
