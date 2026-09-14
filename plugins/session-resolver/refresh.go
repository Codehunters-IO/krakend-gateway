package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

var errSessionGone = errors.New("session gone")

// errInvalidObservedExp guards a misuse class, not a session-state outcome:
// observedExp is supposed to be the Exp value the CALLER already read
// before deciding a refresh was needed (see refresh()'s comment). A caller
// that passes 0 — a forgotten field, a decide() result that dropped Exp —
// would otherwise sail straight through the "is this already fresher than
// what I came here for" check below, since data.Exp > 0 is true for every
// real session. That would silently hand back the unrefreshed,
// about-to-expire token with err == nil: strictly worse than the bug this
// file was fixed for in the previous round, because nothing about the
// return value looks wrong. Failing closed here turns that whole misuse
// class into a 503 instead.
var errInvalidObservedExp = errors.New("observedExp must be a positive unix timestamp")

// releaseScript deletes the refresh lock only if it still holds the value
// this caller wrote when it acquired it — a compare-and-delete, since a
// blind DEL could remove a lock some other caller legitimately acquired
// after this one's TTL had already expired. GET+DEL must be atomic, hence
// Lua rather than two separate calls.
var releaseScript = valkey.NewLuaScript(
	`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`,
)

type refresher struct {
	store    *store
	client   *http.Client
	url      string
	secret   string
	lockTTL  time.Duration
	timeout  time.Duration
	waitStep time.Duration
	maxWait  time.Duration

	// afterAcquire and onAcquireAttempt are test-only seams, nil (no-op) in
	// production. There is no I/O boundary between a caller winning the
	// lock and its post-acquire re-check load — both are Valkey calls
	// inside one synchronous call to refresh(), so no external goroutine
	// can land a write inside that window on any reliable schedule. These
	// let tests observe or act at those two exact points instead.
	//
	// afterAcquire runs once, synchronously, right after this caller wins
	// the lock and before the re-check load — tests use it to inject a
	// write simulating a concurrent refresh finishing in that window.
	//
	// onAcquireAttempt runs at the very start of every acquire() call, win
	// or lose — tests use it to detect whether the lock was ever attempted
	// at all, a fact EXISTS-after-release cannot recover: release deletes
	// the lock whether it was won-then-released or never created, so both
	// read back as 0.
	afterAcquire     func()
	onAcquireAttempt func()
}

func newRefresher(cfg *pluginConfig, s *store) *refresher {
	timeout := time.Duration(cfg.RefreshTimeoutMs) * time.Millisecond
	waitStep := 50 * time.Millisecond
	return &refresher{
		store:    s,
		client:   &http.Client{Timeout: timeout},
		url:      cfg.RefreshURL,
		secret:   cfg.InternalSecret,
		lockTTL:  time.Duration(cfg.RefreshLockTTLSeconds) * time.Second,
		timeout:  timeout,
		waitStep: waitStep,
		// A loser must not give up before the winner's true worst case:
		// hardcoding this below RefreshTimeoutMs turned a slow-but-legitimate
		// round trip into N-1 spurious 503s for a token that was still
		// valid. The winner's worst case is its post-acquire re-check load
		// (bounded by s.timeout) followed by the HTTP call to auth-bff
		// (bounded by timeout) — both must fit before a loser gives up.
		// waitStep on top so a poll fires shortly after that deadline would
		// have been reached, rather than racing it.
		maxWait: timeout + s.timeout + waitStep,
	}
}

// refresh serialises concurrent refreshes for one session behind a Valkey
// lock. The winner calls auth-bff; the losers wait and re-read the token it
// wrote.
//
// observedExp is the Exp value the CALLER already read before deciding a
// refresh was needed (Task 5's handler has this from its own top-level
// load). It is the shared, stable baseline every concurrent caller for the
// same staleness event holds identically — unlike a value refresh() might
// re-derive itself via its own load() call, which races independently
// against however long each caller took to get here and is a DIFFERENT
// value for a caller that simply started late. Every "has someone already
// refreshed past what I came here for" comparison below is against this
// external baseline, never against an internal re-read, for exactly that
// reason: an internal re-read by a late caller can already equal the
// POST-refresh state, at which point no comparison against itself can ever
// show a difference and both the "already fresh, skip the lock" check and
// the "wait for it to change" loop would wait forever or fire immediately
// for the wrong reason.
//
// abs_exp is checked before observedExp's own validity, before the lock is
// ever touched and before auth-bff is ever contacted: a session past its
// absolute ceiling is dead even if a refresh would succeed, so neither the
// lock nor the HTTP call may be reached for it. Tasks 2 and 3 deliberately
// carry no comparison logic — decide() has no expiry fields and
// store.load() returns the raw values — so this is the first point in the
// plugin able to enforce that ordering. Placing the abs_exp check first
// also means a session whose exp/abs_exp are BOTH corrupted (asInt64
// zeroes a malformed field for either) is denied by that check — the
// correct, existing outcome — before ever reaching the observedExp
// validity guard below; that guard exists for a live session called with a
// bad parameter, not to re-litigate a corrupted one.
func (r *refresher) refresh(ctx context.Context, sid string, observedExp int64) (string, error) {
	data, err := r.store.load(ctx, sid)
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", errSessionGone
	}
	// asInt64 (session.go) silently zeroes a malformed abs_exp field. A
	// plain now >= AbsExp comparison then treats a corrupted ceiling as
	// long past, which is the fail-closed direction: 0 can never be a
	// legitimate future epoch, so the ceiling still bites.
	if time.Now().Unix() >= data.AbsExp {
		return "", errSessionGone
	}

	if observedExp <= 0 {
		return "", errInvalidObservedExp
	}

	// Someone may have already refreshed past what this caller observed,
	// in the window between that observation and this call — proactive
	// refresh means many concurrent requests can each independently decide
	// "this needs refreshing" from the same stale snapshot. If the
	// currently-stored Exp is already fresher than that shared baseline,
	// reuse it: no need to touch the lock or call auth-bff at all.
	if data.Exp > observedExp {
		return tokenOrErr(data)
	}

	won, token, err := r.acquire(ctx, sid)
	if err != nil {
		return "", err
	}
	if !won {
		return r.waitForOtherRefresh(ctx, sid, observedExp)
	}
	// Release once this call is done, success or failure, so the next
	// caller for this session does not have to wait out the full lockTTL
	// for work that already finished. lockTTL itself remains the
	// crash-safety backstop for the case where this process dies before
	// reaching here.
	defer r.release(sid, token)

	if r.afterAcquire != nil {
		r.afterAcquire()
	}

	// Re-check against observedExp — the same external, shared baseline,
	// not this caller's own earlier read — now that the lock is held. A
	// caller can win a just-freed lock shortly after some other caller
	// finished and released it, having read stale data before that other
	// refresh's write landed and only reached acquire() after enough
	// scheduling delay for the whole cycle to complete in between. Without
	// this, that caller would call auth-bff a second time for work already
	// done. This is deterministic, not a timing narrowing: the prior
	// winner's Valkey write happens-before its release, which
	// happens-before this acquire succeeded, so if a refresh beat us here,
	// this read observes it, regardless of how much scheduling delay
	// preceded it.
	fresh, err := r.store.load(ctx, sid)
	if err != nil {
		return "", err
	}
	if fresh == nil {
		return "", errSessionGone
	}
	if fresh.Exp > observedExp {
		return tokenOrErr(fresh)
	}

	return r.callAuthBff(ctx, sid)
}

// tokenOrErr guards the same empty-token case callAuthBff's own decode
// step already guards: load() returns (nil, nil) only for a nil Valkey
// reply, not for an empty string field, so a hash with an advanced exp but
// an empty access_token would otherwise yield ("", nil) — a reported
// success carrying no usable token.
func tokenOrErr(data *sessionData) (string, error) {
	if data.AccessToken == "" {
		return "", errors.New("session has no access token")
	}
	return data.AccessToken, nil
}

// acquire returns (true, token, nil) when this caller won the lock, and
// (false, "", nil) — not an error — when SET NX found the key already held.
// valkey-go reports a "not set" NX outcome as a nil reply, surfaced as the
// sentinel error valkey.Nil, so it must be checked before treating err as a
// real failure. token is a random value unique to this acquisition, stored
// as the lock's value so release() can later prove it is deleting the lock
// it acquired rather than one a later caller took after this one's TTL
// expired.
func (r *refresher) acquire(ctx context.Context, sid string) (bool, string, error) {
	if r.onAcquireAttempt != nil {
		r.onAcquireAttempt()
	}

	token, err := randomLockToken()
	if err != nil {
		return false, "", err
	}

	lockCtx, cancel := context.WithTimeout(ctx, r.store.timeout)
	defer cancel()

	cmd := r.store.client.B().Set().Key(r.lockKey(sid)).Value(token).Nx().Ex(r.lockTTL).Build()
	err = r.store.client.Do(lockCtx, cmd).Error()
	if valkey.IsValkeyNil(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, token, nil
}

// release deletes the refresh lock this caller acquired, identified by the
// token it wrote in acquire. It runs on its own short-lived, independent
// context: this is best-effort cleanup that must not be skipped just
// because the caller's own context was already cancelled or its deadline
// consumed by the HTTP call to auth-bff.
func (r *refresher) release(sid, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.store.timeout)
	defer cancel()
	_ = releaseScript.Exec(ctx, r.store.client, []string{r.lockKey(sid)}, []string{token}).Error()
}

func (r *refresher) lockKey(sid string) string {
	return r.store.prefix + "lock:refresh:" + sid
}

func randomLockToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// waitForOtherRefresh polls until Exp strictly exceeds observedExp — the
// same external, shared baseline refresh() checked before ever attempting
// the lock — rather than until Exp is merely in the future. This plugin
// refreshes proactively (Task 2's decide triggers a refresh while the
// token is still near expiry, not after it has expired), so "Exp is in the
// future" is true from the very first poll regardless of whether anyone
// has refreshed yet; only "Exp moved past what I came here for" means the
// winner actually finished. auth-bff rewrites the whole session hash on
// every refresh, so a real refresh always advances exp; nothing else does.
func (r *refresher) waitForOtherRefresh(ctx context.Context, sid string, observedExp int64) (string, error) {
	deadline := time.Now().Add(r.maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(r.waitStep)

		data, err := r.store.load(ctx, sid)
		if err != nil {
			return "", err
		}
		if data == nil {
			return "", errSessionGone
		}
		if data.Exp > observedExp {
			return tokenOrErr(data)
		}
	}
	return "", errors.New("timed out waiting for a concurrent refresh")
}

func (r *refresher) callAuthBff(ctx context.Context, sid string) (string, error) {
	url := strings.ReplaceAll(r.url, "{sid}", sid)

	reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Secret", r.secret)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", errSessionGone
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-bff returned status %d", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", errors.New("auth-bff returned an empty access token")
	}
	return body.AccessToken, nil
}
