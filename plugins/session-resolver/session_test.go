package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/valkey-io/valkey-go"
)

func startValkey(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:9-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start valkey: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	endpoint, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("failed to resolve endpoint: %v", err)
	}
	return endpoint
}

func seed(t *testing.T, addr, sid string, exp, absExp int64) valkey.Client {
	t.Helper()
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("failed to connect to valkey: %v", err)
	}
	t.Cleanup(client.Close)

	ctx := context.Background()
	key := "v1:session:" + sid
	hset := client.B().Hset().Key(key).FieldValue().
		FieldValue("ver", "1").
		FieldValue("sub", "user-1").
		FieldValue("kc_sid", "kc-1").
		FieldValue("access_token", "the.jwt").
		FieldValue("exp", strconv.FormatInt(exp, 10)).
		FieldValue("abs_exp", strconv.FormatInt(absExp, 10)).
		FieldValue("refresh_token_enc", "k1.iv.cipher").
		FieldValue("id_token_enc", "k1.iv.cipher").
		FieldValue("created_at", strconv.FormatInt(exp-300, 10)).
		Build()
	if err := client.Do(ctx, hset).Error(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	if err := client.Do(ctx, client.B().Expire().Key(key).Seconds(1800).Build()).Error(); err != nil {
		t.Fatalf("expire failed: %v", err)
	}
	return client
}

func ttlOf(t *testing.T, client valkey.Client, key string) time.Duration {
	t.Helper()
	seconds, err := client.Do(context.Background(), client.B().Ttl().Key(key).Build()).AsInt64()
	if err != nil {
		t.Fatalf("TTL failed: %v", err)
	}
	return time.Duration(seconds) * time.Second
}

func TestStoreLoad(t *testing.T) {
	addr := startValkey(t)
	sid := "B123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+300, now+36000)

	s, err := newStore(&pluginConfig{ValkeyAddr: addr, KeyPrefix: "v1:", ValkeyTimeoutMs: 500, IdleTTLSeconds: 1800})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	ctx := context.Background()

	t.Run("reads the four contract fields", func(t *testing.T) {
		data, err := s.load(ctx, sid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data == nil {
			t.Fatal("expected a session, got nil")
		}
		if data.AccessToken != "the.jwt" {
			t.Errorf("access token = %q, want the.jwt", data.AccessToken)
		}
		if data.Subject != "user-1" {
			t.Errorf("subject = %q, want user-1", data.Subject)
		}
		if data.Exp != now+300 {
			t.Errorf("exp = %d, want %d", data.Exp, now+300)
		}
		if data.AbsExp != now+36000 {
			t.Errorf("abs_exp = %d, want %d", data.AbsExp, now+36000)
		}
	})

	t.Run("returns nil for a missing session", func(t *testing.T) {
		data, err := s.load(ctx, "C123456789012345678901234567890123456789012")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data != nil {
			t.Errorf("expected nil for a missing session, got %+v", data)
		}
	})

	t.Run("errors when valkey is unreachable", func(t *testing.T) {
		// newStore still succeeds: ForceSingleClient makes valkey.NewClient
		// return a usable, self-reconnecting client even though the initial
		// dial fails. The failure must surface on load(), not construction.
		dead, err := newStore(&pluginConfig{ValkeyAddr: "127.0.0.1:1", KeyPrefix: "v1:", ValkeyTimeoutMs: 100})
		if err != nil {
			t.Fatalf("newStore should tolerate an unreachable address at construction time: %v", err)
		}
		if _, err := dead.load(ctx, sid); err == nil {
			t.Fatal("expected an error when valkey is unreachable")
		}
	})
}

func TestStoreRenewIdleOnlyBelowHalf(t *testing.T) {
	addr := startValkey(t)
	sid := "D123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+300, now+36000)
	ctx := context.Background()

	s, err := newStore(&pluginConfig{ValkeyAddr: addr, KeyPrefix: "v1:", ValkeyTimeoutMs: 500, IdleTTLSeconds: 1800})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	key := "v1:session:" + sid

	// TTL is 30m — above half, so renewal must be a no-op write-wise.
	client.Do(ctx, client.B().Expire().Key(key).Seconds(1800).Build())
	s.renewIdle(ctx, sid)
	if ttl := ttlOf(t, client, key); ttl < 29*time.Minute {
		t.Errorf("ttl = %v, expected it left untouched near 30m", ttl)
	}

	// Drop below half: renewal must push it back to the full idle TTL.
	client.Do(ctx, client.B().Expire().Key(key).Seconds(300).Build())
	s.renewIdle(ctx, sid)
	if ttl := ttlOf(t, client, key); ttl < 29*time.Minute {
		t.Errorf("ttl = %v, expected it renewed to ~30m", ttl)
	}

	// The reverse index belongs to auth-bff and carries a TTL running to
	// abs_exp. Renewal must not shorten it to the idle window: if it does, the
	// index expires under a live session and backchannel logout silently
	// no-ops while answering Keycloak 200 OK. Nothing else in either
	// repository catches that, so it is asserted here.
	index := "v1:kcsid:kc-1"
	client.Do(ctx, client.B().Set().Key(index).Value(sid).Ex(10*time.Hour).Build())
	s.renewIdle(ctx, sid)
	if ttl := ttlOf(t, client, index); ttl < 9*time.Hour {
		t.Errorf("kcsid index ttl = %v, want it left near 10h — renewIdle must not touch the index", ttl)
	}
}

func TestStoreDrop(t *testing.T) {
	addr := startValkey(t)
	sid := "E123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+300, now+36000)
	ctx := context.Background()

	s, err := newStore(&pluginConfig{ValkeyAddr: addr, KeyPrefix: "v1:", ValkeyTimeoutMs: 500, IdleTTLSeconds: 1800})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	s.drop(ctx, sid)

	n, err := client.Do(ctx, client.B().Exists().Key("v1:session:"+sid).Build()).AsInt64()
	if err != nil {
		t.Fatalf("EXISTS failed: %v", err)
	}
	if n != 0 {
		t.Errorf("session key still exists after drop")
	}
}
