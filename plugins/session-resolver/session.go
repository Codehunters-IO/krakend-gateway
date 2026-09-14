package main

import (
	"context"
	"net"
	"time"

	"github.com/valkey-io/valkey-go"
)

// sessionData is the read projection of v1:session:{sid}. The *_enc fields are
// deliberately absent: they are encrypted with a key only auth-bff holds, and the
// edge has no business decrypting them.
type sessionData struct {
	AccessToken string
	Subject     string
	Exp         int64
	AbsExp      int64
}

type store struct {
	client  valkey.Client
	prefix  string
	timeout time.Duration
	idleTTL time.Duration
}

// newStore returns an error only for real misconfiguration. valkey.NewClient
// dials eagerly to probe cluster topology, but ForceSingleClient (always set
// here: this plugin only ever talks to one standalone Valkey node, never a
// cluster) makes it return a usable, self-reconnecting client even when that
// initial dial fails — alongside a non-nil error carrying the dial failure.
// A nil client is the only case that means real misconfiguration (e.g. an
// empty address, already rejected by parseConfig), so that is the only case
// treated as fatal here; the transient dial error is otherwise discarded and
// left to resurface on the first load().
func newStore(cfg *pluginConfig) (*store, error) {
	timeout := time.Duration(cfg.ValkeyTimeoutMs) * time.Millisecond
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{cfg.ValkeyAddr},
		Password:          cfg.ValkeyPassword,
		SelectDB:          cfg.ValkeyDB,
		Dialer:            net.Dialer{Timeout: timeout},
		ConnWriteTimeout:  timeout,
		ForceSingleClient: true,
	})
	if client == nil {
		return nil, err
	}
	return &store{
		client:  client,
		prefix:  cfg.KeyPrefix,
		timeout: timeout,
		idleTTL: time.Duration(cfg.IdleTTLSeconds) * time.Second,
	}, nil
}

// load returns (nil, nil) when the session does not exist, and a non-nil error
// only when Valkey itself failed — the caller distinguishes 401 from 503 on
// that. HMGET always replies with an array the length of the requested field
// list, even on a missing key (each element nil); a missing session is
// detected from the first field (access_token) being a nil reply, not from
// the array itself being nil.
func (s *store) load(ctx context.Context, sid string) (*sessionData, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := s.client.B().Hmget().Key(s.key(sid)).Field("access_token", "sub", "exp", "abs_exp").Build()
	values, err := s.client.Do(ctx, cmd).ToArray()
	if err != nil {
		return nil, err
	}
	if len(values) != 4 {
		return nil, nil
	}

	accessToken, err := values[0].ToString()
	if valkey.IsValkeyNil(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &sessionData{
		AccessToken: accessToken,
		Subject:     asString(values[1]),
		Exp:         asInt64(values[2]),
		AbsExp:      asInt64(values[3]),
	}, nil
}

// renewIdle extends the key's TTL only once less than half of it remains, so an
// active session costs one write every ~15 minutes instead of one per request.
//
// It touches v1:session:{sid} and nothing else. The reverse index
// v1:kcsid:{kc_sid} is auth-bff's to manage and carries a TTL that runs to the
// session's abs_exp; applying the idle TTL to it here would shorten it on every
// renewal, letting it expire under a live session and silently breaking
// backchannel logout — which would then no-op and answer Keycloak 200 OK.
func (s *store) renewIdle(ctx context.Context, sid string) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	ttlSeconds, err := s.client.Do(ctx, s.client.B().Ttl().Key(s.key(sid)).Build()).AsInt64()
	if err != nil || ttlSeconds <= 0 {
		return
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	if ttl*2 < s.idleTTL {
		expire := s.client.B().Expire().Key(s.key(sid)).Seconds(int64(s.idleTTL.Seconds())).Build()
		_ = s.client.Do(ctx, expire).Error()
	}
}

func (s *store) drop(ctx context.Context, sid string) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_ = s.client.Do(ctx, s.client.B().Del().Key(s.key(sid)).Build()).Error()
}

func (s *store) key(sid string) string { return s.prefix + "session:" + sid }

// asString and asInt64 tolerate a nil or unexpected reply by returning the
// zero value, matching the original defensive decoding: a corrupted record
// should not panic the request path.
func asString(m valkey.ValkeyMessage) string {
	v, err := m.ToString()
	if err != nil {
		return ""
	}
	return v
}

func asInt64(m valkey.ValkeyMessage) int64 {
	v, err := m.AsInt64()
	if err != nil {
		return 0
	}
	return v
}
