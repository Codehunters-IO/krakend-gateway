package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

// aclUser is the least privilege the plugin can run with: the four commands it
// issues (session.go), scoped to its own key prefix, plus the connection
// commands valkey-go needs to complete a handshake.
//
// Measured, not read off a page: with the four data commands alone the
// resolver answers 503 on every request. valkey-go opens with HELLO and
// enables client-side caching with CLIENT TRACKING, so a user that cannot run
// those never gets a usable connection — it fails closed, but for a reason
// that reads like a store outage.
const (
	aclUserName = "gateway"
	aclPassword = "acl-test-password"
	aclGrants   = "~v1:session:* +hmget +ttl +expire +del +@connection"
)

// createACLUser provisions the restricted user through the default (nopass)
// user of the test container.
func createACLUser(t *testing.T, addr string) {
	t.Helper()

	admin, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("connecting as default: %v", err)
	}
	defer admin.Close()

	args := append([]string{"ACL", "SETUSER", aclUserName, "on", ">" + aclPassword},
		strings.Fields(aclGrants)...)
	if err := admin.Do(context.Background(), admin.B().Arbitrary(args...).Build()).Error(); err != nil {
		t.Fatalf("ACL SETUSER: %v", err)
	}
}

// disableDefaultUser is load-bearing, and the reason is worth stating. Valkey's
// `default` user ships as `nopass`, and a nopass user accepts ANY password. So
// a client that sends only a password — which is what this plugin does when no
// username is configured — authenticates successfully as `default`, with every
// permission on every key. A test that merely passes with the ACL user
// configured therefore proves nothing: it passes identically when the username
// is ignored. Mutation testing showed exactly that. Turning `default` off makes
// the restricted user the only way in.
func disableDefaultUser(t *testing.T, addr string) {
	t.Helper()

	admin, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("connecting as default: %v", err)
	}
	defer admin.Close()

	err = admin.Do(context.Background(),
		admin.B().Arbitrary("ACL", "SETUSER", "default", "off").Build()).Error()
	if err != nil {
		t.Fatalf("disabling the default user: %v", err)
	}
}

func aclConfig(addr string) map[string]interface{} {
	cfg := handlerConfig(addr)
	cfg["valkey_username"] = aclUserName
	cfg["valkey_password"] = aclPassword
	return cfg
}

// The point of the whole exercise: the plugin does its job as a user that
// cannot read anything but its own sessions.
func TestTheResolverWorksAsARestrictedACLUser(t *testing.T) {
	addr := startValkey(t)
	createACLUser(t, addr)

	sid := strings.Repeat("a", 43)
	seed(t, addr, sid, time.Now().Add(time.Hour).Unix(), time.Now().Add(10*time.Hour).Unix())
	disableDefaultUser(t, addr) // after seeding: seed() connects as default

	handler, captured := buildHandler(t, aclConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the restricted user could not resolve a session", rec.Code)
	}
	if !captured.Called {
		t.Fatal("the request never reached the backend")
	}
	if captured.Authorization != "Bearer the.jwt" {
		t.Fatalf("Authorization = %q, want the seeded token", captured.Authorization)
	}
}

// Least privilege has to be real, not decorative: the same credentials must be
// unable to reach anything outside the session prefix, or the ACL is theatre.
func TestTheRestrictedUserCannotReachBeyondItsPrefix(t *testing.T) {
	addr := startValkey(t)
	createACLUser(t, addr)

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		Username:          aclUserName,
		Password:          aclPassword,
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("the restricted user could not connect at all: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	for name, cmd := range map[string][]string{
		"a key outside the prefix":       {"HMGET", "v1:lock:refresh:abc", "x"},
		"another application's key":      {"GET", "some:other:key"},
		"flushing the store":             {"FLUSHALL"},
		"reading the whole session hash": {"HGETALL", "v1:session:" + strings.Repeat("a", 43)},
		"writing a session field":        {"HSET", "v1:session:" + strings.Repeat("a", 43), "access_token", "forged"},
		"listing keys":                   {"KEYS", "*"},
	} {
		t.Run(name, func(t *testing.T) {
			err := client.Do(ctx, client.B().Arbitrary(cmd...).Build()).Error()
			if err == nil {
				t.Fatalf("%v succeeded; the ACL does not constrain the edge", cmd)
			}
			if !strings.Contains(err.Error(), "NOPERM") {
				t.Fatalf("expected a NOPERM denial, got %v", err)
			}
		})
	}
}

// An empty username is legacy AUTH, which is what every existing deployment
// uses today. It must keep working, or this change is a breaking one.
func TestAnEmptyUsernameStillAuthenticatesAsBefore(t *testing.T) {
	addr := startValkey(t)

	sid := strings.Repeat("b", 43)
	seed(t, addr, sid, time.Now().Add(time.Hour).Unix(), time.Now().Add(10*time.Hour).Unix())

	cfg := handlerConfig(addr)
	cfg["valkey_username"] = ""

	handler, captured := buildHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !captured.Called {
		t.Fatal("the request never reached the backend")
	}
}

// Wrong credentials must fail closed, not fall back to an anonymous or default
// connection.
func TestWrongACLCredentialsFailClosed(t *testing.T) {
	addr := startValkey(t)
	createACLUser(t, addr)

	sid := strings.Repeat("c", 43)
	seed(t, addr, sid, time.Now().Add(time.Hour).Unix(), time.Now().Add(10*time.Hour).Unix())

	disableDefaultUser(t, addr) // or a wrong password still gets in as `default`

	cfg := aclConfig(addr)
	cfg["valkey_password"] = "not-the-password"

	handler, captured := buildHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a store it cannot authenticate against", rec.Code)
	}
	if captured.Called {
		t.Fatal("the request reached the backend with no session resolved")
	}
}
