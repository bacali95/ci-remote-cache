package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ci-remote-cache/internal/store"
)

type fakeAuthenticator struct {
	byHash  map[string]store.Token
	failErr error // if set, Authenticate always returns this instead of looking up byHash
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, tokenHash string) (store.Token, error) {
	if f.failErr != nil {
		return store.Token{}, f.failErr
	}
	tok, ok := f.byHash[tokenHash]
	if !ok {
		return store.Token{}, store.ErrNotFound
	}
	return tok, nil
}

func TestCacheTokenAuthAuthorizePropagatesUnexpectedError(t *testing.T) {
	boom := errors.New("boom")
	a := NewCacheTokenAuth(&fakeAuthenticator{failErr: boom})

	_, _, err := a.Authorize(context.Background(), "any-token", ScopeRead)
	if !errors.Is(err, boom) {
		t.Fatalf("Authorize error = %v, want %v", err, boom)
	}
}

func TestCacheTokenAuthAuthorize(t *testing.T) {
	fake := &fakeAuthenticator{byHash: map[string]store.Token{
		HashToken("reader"): {Scope: store.ScopeRead},
		HashToken("writer"): {Scope: store.ScopeWrite},
	}}
	a := NewCacheTokenAuth(fake)
	ctx := context.Background()

	cases := []struct {
		name        string
		token       string
		scope       Scope
		wantValid   bool
		wantAllowed bool
	}{
		{"reader can read", "reader", ScopeRead, true, true},
		{"reader cannot write", "reader", ScopeWrite, true, false},
		{"writer can read", "writer", ScopeRead, true, true},
		{"writer can write", "writer", ScopeWrite, true, true},
		{"unknown token", "nope", ScopeRead, false, false},
		{"empty token", "", ScopeRead, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, allowed, err := a.Authorize(ctx, tc.token, tc.scope)
			if err != nil {
				t.Fatalf("Authorize error: %v", err)
			}
			if valid != tc.wantValid || allowed != tc.wantAllowed {
				t.Fatalf("Authorize(%q) = (%v, %v), want (%v, %v)", tc.token, valid, allowed, tc.wantValid, tc.wantAllowed)
			}
		})
	}
}

func TestHashTokenDeterministicAndDistinct(t *testing.T) {
	first := HashToken("abc")
	second := HashToken("abc")
	if first != second {
		t.Fatalf("HashToken not deterministic")
	}
	if first == HashToken("abd") {
		t.Fatalf("HashToken collided for distinct inputs")
	}
}

func TestGenerateToken(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(tok) < 20 {
		t.Fatalf("token too short: %q", tok)
	}
	const prefix = "crc_"
	if tok[:len(prefix)] != prefix {
		t.Fatalf("token missing prefix: %q", tok)
	}

	tok2, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok == tok2 {
		t.Fatalf("two calls to GenerateToken produced the same token")
	}
}

// GenerateToken's crypto/rand.Read error branch is intentionally not
// covered: since Go 1.24, a failing system entropy source makes
// crypto/rand call runtime.fatal (an unrecoverable process crash), not
// return an error — see https://go.dev/issue/66821. The `if err != nil`
// check is defensive for older/alternate runtimes but can't be exercised
// from a test on this Go version.

func TestExtractBearer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := ExtractBearer(r); got != "" {
		t.Fatalf("no header: got %q, want empty", got)
	}

	r.Header.Set("Authorization", "Bearer abc123")
	if got := ExtractBearer(r); got != "abc123" {
		t.Fatalf("got %q, want abc123", got)
	}

	r.Header.Set("Authorization", "Basic abc123")
	if got := ExtractBearer(r); got != "" {
		t.Fatalf("non-bearer scheme: got %q, want empty", got)
	}
}
