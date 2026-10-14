package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"codeberg.org/example/snapvault/internal/platform/httpx"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeIssuer is a minimal OIDC provider: a discovery document and a JWKS
// endpoint, plus the private key to sign tokens with.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                fi.srv.URL,
			"jwks_uri":                              fi.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig",
		}}})
	})
	fi.srv = httptest.NewServer(mux)
	t.Cleanup(fi.srv.Close)
	return fi
}

// token signs claims with key (the issuer's own key unless overridden).
func (fi *fakeIssuer) token(t *testing.T, key *rsa.PrivateKey, claims jwt.Claims) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (fi *fakeIssuer) claims(sub, aud string, exp time.Time) jwt.Claims {
	return jwt.Claims{
		Issuer:   fi.srv.URL,
		Subject:  sub,
		Audience: jwt.Audience{aud},
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		Expiry:   jwt.NewNumericDate(exp),
	}
}

func withBearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/images", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestOIDCAuth_verifiesTokens(t *testing.T) {
	fi := newFakeIssuer(t)
	auth, err := newOIDCAuth(context.Background(), fi.srv.URL, "snapvault-prod")
	if err != nil {
		t.Fatalf("newOIDCAuth: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)

	valid := fi.token(t, fi.key, fi.claims("user-123", "snapvault-prod", future))
	if got, err := auth.UserID(withBearer(valid)); err != nil || got != "user-123" {
		t.Fatalf("valid token: got (%q, %v), want (%q, nil)", got, err, "user-123")
	}

	wrongIssuer := fi.claims("user-123", "snapvault-prod", future)
	wrongIssuer.Issuer = "https://evil.example"

	rejected := map[string]string{
		"wrong audience": fi.token(t, fi.key, fi.claims("user-123", "another-project", future)),
		"wrong issuer":   fi.token(t, fi.key, wrongIssuer),
		"expired":        fi.token(t, fi.key, fi.claims("user-123", "snapvault-prod", time.Now().Add(-time.Minute))),
		"foreign key":    fi.token(t, otherKey, fi.claims("user-123", "snapvault-prod", future)),
		"no subject":     fi.token(t, fi.key, fi.claims("", "snapvault-prod", future)),
		"garbage":        "not-a-jwt",
	}
	for name, tok := range rejected {
		t.Run(name, func(t *testing.T) {
			if got, err := auth.UserID(withBearer(tok)); err == nil {
				t.Fatalf("accepted as %q, want an error", got)
			}
		})
	}

	noHeader := httptest.NewRequest(http.MethodPost, "/api/images", nil)
	if _, err := auth.UserID(noHeader); !errors.Is(err, errNoToken) {
		t.Fatalf("no Authorization header: err = %v, want errNoToken", err)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
		ok     bool
	}{
		"bearer":        {"Bearer abc", "abc", true},
		"lowercase":     {"bearer abc", "abc", true},
		"basic":         {"Basic abc", "", false},
		"empty token":   {"Bearer ", "", false},
		"scheme only":   {"Bearer", "", false},
		"missing":       {"", "", false},
		"extra spacing": {"Bearer   abc ", "abc", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			got, ok := bearerToken(r)
			if got != c.want || ok != c.ok {
				t.Fatalf("bearerToken(%q) = (%q, %v), want (%q, %v)", c.header, got, ok, c.want, c.ok)
			}
		})
	}
}

type fakeAuth struct {
	user string
	err  error
}

func (f fakeAuth) UserID(*http.Request) (string, error) { return f.user, f.err }

// upstream records the headers the protected handler receives.
func upstream(seen *http.Header) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	})
}

func TestRequireUser_rejectsWithoutCredentials(t *testing.T) {
	var seen http.Header
	h := requireUser(fakeAuth{err: errNoToken}, discardLog, upstream(&seen))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/images", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 without a WWW-Authenticate header")
	}
	if seen != nil {
		t.Error("upstream was called for an unauthenticated request")
	}
}

func TestRequireUser_passesVerifiedUserNotToken(t *testing.T) {
	var seen http.Header
	h := requireUser(fakeAuth{user: "user-123"}, discardLog, upstream(&seen))

	h.ServeHTTP(httptest.NewRecorder(), withBearer("secret-token"))

	if got := seen.Get(httpx.UserHeader); got != "user-123" {
		t.Errorf("%s = %q, want %q", httpx.UserHeader, got, "user-123")
	}
	if got := seen.Get("Authorization"); got != "" {
		t.Errorf("token forwarded upstream: Authorization = %q", got)
	}
}

func TestStripInternalHeaders_clientCannotClaimAUser(t *testing.T) {
	var seen http.Header
	// The full chain as main builds it: strip first, then authenticate.
	h := stripInternalHeaders(requireUser(fakeAuth{user: "real-user"}, discardLog, upstream(&seen)))

	r := withBearer("token")
	r.Header.Set(httpx.UserHeader, "someone-else")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got := seen.Get(httpx.UserHeader); got != "real-user" {
		t.Fatalf("%s = %q, want the verified user %q", httpx.UserHeader, got, "real-user")
	}

	// Routes without authentication must not pass a forged header through.
	seen = nil
	open := stripInternalHeaders(upstream(&seen))
	r = httptest.NewRequest(http.MethodGet, "/api/feed", nil)
	r.Header.Set(httpx.UserHeader, "someone-else")
	open.ServeHTTP(httptest.NewRecorder(), r)
	if got := seen.Get(httpx.UserHeader); got != "" {
		t.Fatalf("forged %s reached upstream: %q", httpx.UserHeader, got)
	}
}

func TestNewAuthFromEnv_failsClosed(t *testing.T) {
	t.Setenv("AUTH_ISSUER", "")
	t.Setenv("AUTH_DEV_USER", "")
	if _, err := newAuthFromEnv(context.Background(), discardLog); err == nil {
		t.Fatal("no auth configured: want an error, got an authenticator")
	}

	t.Setenv("AUTH_ISSUER", "https://issuer.example")
	t.Setenv("AUTH_AUDIENCE", "")
	if _, err := newAuthFromEnv(context.Background(), discardLog); err == nil {
		t.Fatal("issuer without audience: want an error")
	}

	t.Setenv("AUTH_ISSUER", "")
	t.Setenv("AUTH_DEV_USER", "dev-user")
	auth, err := newAuthFromEnv(context.Background(), discardLog)
	if err != nil {
		t.Fatalf("dev mode: %v", err)
	}
	if got, _ := auth.UserID(httptest.NewRequest(http.MethodPost, "/", nil)); got != "dev-user" {
		t.Fatalf("dev mode user = %q, want %q", got, "dev-user")
	}
}
