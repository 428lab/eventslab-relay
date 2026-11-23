package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxCodeAttempts: after this many wrong codes the pairing code is regenerated (design §4.3).
const maxCodeAttempts = 5

// Wrong codes also slow guessing down: from the lockoutAfter-th wrong code in a row, pairing is
// refused for lockoutBase, doubling with each further wrong code up to lockoutMax. A right code
// resets it. Any local program can send any Origin header, so this is what bounds blind guessing.
const (
	lockoutAfter = 3
	lockoutBase  = time.Second
	lockoutMax   = 30 * time.Second
)

// lockedError: pairing is locked for wait after too many wrong codes.
type lockedError struct{ wait time.Duration }

func (e *lockedError) Error() string { return fmt.Sprintf("too many wrong codes; retry in %s", e.wait) }

// pairing holds the 6-digit code shown in the console and the tokens issued to origins.
// Tokens are stored hashed (SHA-256) in the user's config dir, so pairing survives restarts.
type pairing struct {
	mu       sync.Mutex
	code     string
	attempts int
	path     string            // tokens file; "" = memory only (tests)
	tokens   map[string]string // sha256(token) hex -> origin
	onCode   func(code string) // called when a new code is made (console)

	wrongRun    int       // wrong codes in a row (not reset by a new code)
	lockedUntil time.Time // no pairing before this
	now         func() time.Time
}

type tokenFile struct {
	Tokens map[string]string `json:"tokens"` // sha256(token) hex -> origin
}

// configDir is <user config dir>/eventslab-relay (e.g. ~/Library/Application Support on macOS,
// %AppData% on Windows).
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "eventslab-relay"), nil
}

func newPairing(path string, onCode func(string)) (*pairing, error) {
	p := &pairing{path: path, tokens: map[string]string{}, onCode: onCode, now: time.Now}
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			var f tokenFile
			if err := json.Unmarshal(b, &f); err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			if f.Tokens != nil {
				p.tokens = f.Tokens
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	p.newCode()
	return p, nil
}

// newCode makes a fresh 6-digit code; the caller must hold mu or be the constructor.
func (p *pairing) newCode() {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	p.code = fmt.Sprintf("%06d", n.Int64())
	p.attempts = 0
	if p.onCode != nil {
		p.onCode(p.code)
	}
}

func (p *pairing) Code() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func (p *pairing) Paired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokens) > 0
}

var errBadCode = errors.New("wrong pairing code")

// Pair checks the code and issues a token for origin. A wrong code counts toward a reset.
func (p *pairing) Pair(origin, code string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := p.now(); now.Before(p.lockedUntil) {
		return "", &lockedError{wait: p.lockedUntil.Sub(now)}
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) != 1 {
		p.attempts++
		p.wrongRun++
		if p.attempts >= maxCodeAttempts {
			p.newCode()
		}
		if p.wrongRun >= lockoutAfter {
			wait := lockoutBase << min(p.wrongRun-lockoutAfter, 5)
			p.lockedUntil = p.now().Add(min(wait, lockoutMax))
		}
		return "", errBadCode
	}
	p.wrongRun, p.lockedUntil = 0, time.Time{}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	p.tokens[hashToken(token)] = origin
	if err := p.saveLocked(); err != nil {
		delete(p.tokens, hashToken(token))
		return "", err
	}
	p.newCode() // a code is single-use
	return token, nil
}

// Check reports whether token was issued to origin.
func (p *pairing) Check(origin, token string) bool {
	if token == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.tokens[hashToken(token)]
	return ok && o == origin
}

// Reset forgets every token (the console's "つなぎ直す").
func (p *pairing) Reset() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = map[string]string{}
	p.newCode()
	return p.saveLocked()
}

func (p *pairing) saveLocked() error {
	if p.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tokenFile{Tokens: p.tokens}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}
