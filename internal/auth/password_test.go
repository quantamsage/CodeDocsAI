package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(fastParams, 2)
	ctx := context.Background()

	hash, err := h.Hash(ctx, "s3cret passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("unexpected format: %q", hash)
	}
	if rehash, err := h.Verify(ctx, "s3cret passphrase", hash); err != nil || rehash {
		t.Errorf("Verify correct password: rehash=%v err=%v", rehash, err)
	}
	if _, err := h.Verify(ctx, "s3cret passphrasE", hash); !errors.Is(err, ErrMismatchedPassword) {
		t.Errorf("Verify wrong password err = %v", err)
	}

	other, _ := h.Hash(ctx, "s3cret passphrase")
	if other == hash {
		t.Error("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyFlagsOutdatedParams(t *testing.T) {
	ctx := context.Background()
	old := NewHasher(Argon2Params{Memory: 32, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 1)
	hash, _ := old.Hash(ctx, "pw")

	rehash, err := NewHasher(fastParams, 1).Verify(ctx, "pw", hash)
	if err != nil || !rehash {
		t.Errorf("rehash=%v err=%v; want true, nil", rehash, err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := NewHasher(fastParams, 1)
	for _, bad := range []string{
		"",
		"plaintext",
		"$2a$10$abcdefghijklmnopqrstuv", // bcrypt
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", // wrong variant
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", // absurd memory
		"$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$c2hvcnQ", // key too short
	} {
		if _, err := h.Verify(context.Background(), "pw", bad); err == nil || errors.Is(err, ErrMismatchedPassword) {
			t.Errorf("Verify(%q) err = %v, want malformed-hash error", bad, err)
		}
	}
}

func TestHasherRespectsContextWhenSaturated(t *testing.T) {
	h := NewHasher(fastParams, 1)
	h.sem <- struct{}{} // occupy the only slot
	defer h.release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "pw"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

func TestThrottleWindow(t *testing.T) {
	clock := newClock()
	th := NewThrottle(2, time.Minute, clock.Now)

	if blocked, _ := th.Blocked("k"); blocked {
		t.Fatal("blocked before any failure")
	}
	th.Fail("k")
	if blocked, _ := th.Blocked("k"); blocked {
		t.Fatal("blocked below limit")
	}
	th.Fail("k")
	blocked, wait := th.Blocked("k")
	if !blocked || wait != time.Minute {
		t.Fatalf("blocked=%v wait=%v", blocked, wait)
	}
	if blocked, _ := th.Blocked("other"); blocked {
		t.Error("keys must be independent")
	}

	clock.Advance(time.Minute)
	if blocked, _ := th.Blocked("k"); blocked {
		t.Error("still blocked after window")
	}
	if len(th.entries) != 0 {
		t.Errorf("expired entries not swept: %d", len(th.entries))
	}

	th.Fail("k")
	th.Fail("k")
	th.Reset("k")
	if blocked, _ := th.Blocked("k"); blocked {
		t.Error("blocked after Reset")
	}
}
