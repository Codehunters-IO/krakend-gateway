package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

var errSessionGone = errors.New("session gone")

// releaseScript shortens the refresh lock's TTL to a brief grace window
// instead of deleting it outright, and only if it still holds the value
// this caller wrote when it acquired it — a blind release could otherwise
// touch a lock some other caller legitimately acquired after this one's
// TTL had already expired. GET+PEXPIRE must be atomic, hence Lua.
//
// A grace window rather than an immediate delete matters for a different
// reason than the token check: a straggler can read stale data (correctly,
// honestly stale — this is not the observedExp race, see refresh()'s
// comment) and then be descheduled by the Go runtime between that read and
// its own acquire() attempt for long enough that the actual winner's whole
// cycle, release included, completes in between. An immediate delete lets
// that straggler's late acquire() succeed and call auth-bff a second time.
// Keeping the lock present a little longer makes SET NX correctly reject
// that attempt instead, routing it into waitForOtherRefresh — which, using
// the same external observedExp baseline, correctly hands back the fresh
// token rather than timing out. lockTTL remains the crash-safety backstop
// for a winner that never reaches release at all.
var releaseScript = valkey.NewLuaScript(
	`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("pexpire", KEYS[1], ARGV[2]) else return 0 end`,
)

type refresher struct {
	store        *store
	client       *http.Client
	url          string
	secret       string
	lockTTL      time.Duration
	timeout      time.Duration
	waitStep     time.Duration
	maxWait      time.Duration
	releaseGrace time.Duration
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
		maxWait:  500 * time.Millisecond,
		// Comfortably longer than realistic goroutine-scheduling delay
		// under contention, far shorter than lockTTL.
		releaseGrace: 5 * waitStep,
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
// for the wrong reason. This was found empirically while stabilising
// TestRefresh's concurrency subtest — see the fix report for the two
// narrower attempts (comparing against refresh()'s own internal read, and
// a short post-release grace window) that this replaced, and why each
// still left a reproducible gap.
//
// abs_exp is checked before observedExp, before the lock is ever touched
// and before auth-bff is ever contacted: a session past its absolute
// ceiling is dead even if a refresh would succeed, so neither the lock nor
// the HTTP call may be reached for it. Tasks 2 and 3 deliberately carry no
// comparison logic — decide() has no expiry fields and store.load()
// returns the raw values — so this is the first point in the plugin able
// to enforce that ordering.
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

	// Someone may have already refreshed past what this caller observed,
	// in the window between that observation and this call — proactive
	// refresh means many concurrent requests can each independently decide
	// "this needs refreshing" from the same stale snapshot. If the
	// currently-stored Exp is already fresher than that shared baseline,
	// reuse it: no need to touch the lock or call auth-bff at all.
	if data.Exp > observedExp {
		return data.AccessToken, nil
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
	// for work that already finished. See releaseScript's comment for why
	// this shortens the lock's TTL to a brief grace window rather than
	// deleting it outright. lockTTL itself remains the crash-safety
	// backstop for the case where this process dies before reaching here.
	defer r.release(sid, token)
	return r.callAuthBff(ctx, sid)
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

// release shortens the refresh lock this caller acquired down to a brief
// grace window, identified by the token it wrote in acquire. It runs on its
// own short-lived, independent context: this is best-effort cleanup that
// must not be skipped just because the caller's own context was already
// cancelled or its deadline consumed by the HTTP call to auth-bff.
func (r *refresher) release(sid, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.store.timeout)
	defer cancel()
	graceMs := strconv.FormatInt(r.releaseGrace.Milliseconds(), 10)
	_ = releaseScript.Exec(ctx, r.store.client, []string{r.lockKey(sid)}, []string{token, graceMs}).Error()
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
			return data.AccessToken, nil
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
