package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the argon2id cost parameters.
type Argon2Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultArgon2Params follow the OWASP Password Storage Cheat Sheet
// recommendation for argon2id (19 MiB, 2 iterations, 1 degree of parallelism).
var DefaultArgon2Params = Argon2Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// ErrMismatchedPassword is returned when a password does not match a hash.
var ErrMismatchedPassword = errors.New("auth: password does not match")

// errMalformedHash is returned for stored hashes that cannot be parsed.
var errMalformedHash = errors.New("auth: malformed password hash")

// Hasher hashes and verifies passwords with argon2id. Hashes are stored in the
// PHC string format, so the parameters travel with each hash and can be raised
// later without invalidating existing passwords.
type Hasher struct {
	params Argon2Params
	// sem bounds concurrent hashing: each argon2 call allocates params.Memory
	// KiB, so unbounded concurrency would let a burst of logins exhaust memory.
	sem chan struct{}
}

// NewHasher returns a Hasher using params, allowing at most concurrency
// simultaneous hash computations.
func NewHasher(params Argon2Params, concurrency int) *Hasher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Hasher{params: params, sem: make(chan struct{}, concurrency)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns a PHC-formatted argon2id hash of password with a random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)
	h.release()

	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded. It returns
// ErrMismatchedPassword on mismatch and another error if encoded is malformed.
// needsRehash is true when the hash was made with different parameters than
// the Hasher's current ones.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (needsRehash bool, err error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	h.release()

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, ErrMismatchedPassword
	}
	cur := h.params
	needsRehash = p.Memory != cur.Memory || p.Time != cur.Time || p.Threads != cur.Threads ||
		uint32(len(salt)) != cur.SaltLen || uint32(len(want)) != cur.KeyLen
	return needsRehash, nil
}

func decodeHash(encoded string) (p Argon2Params, salt, key []byte, err error) {
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errMalformedHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, errMalformedHash
	}
	// Refuse absurd parameters from a tampered row rather than allocating them.
	if p.Memory == 0 || p.Memory > 1<<21 || p.Time == 0 || p.Time > 100 || p.Threads == 0 {
		return p, nil, nil, errMalformedHash
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return p, nil, nil, errMalformedHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) < 16 || len(key) > 128 {
		return p, nil, nil, errMalformedHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}
