package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"codeberg.org/example/snapvault/internal/platform/config"
	"codeberg.org/example/snapvault/internal/platform/httpx"
)

// authenticator resolves who is making a request.
type authenticator interface {
	// UserID returns the verified user behind r, or an error if r carries no
	// valid credentials.
	UserID(r *http.Request) (string, error)
}

var errNoToken = errors.New("missing bearer token")

// oidcAuth verifies OIDC ID tokens: signature against the issuer's published
// keys, issuer, audience, and expiry. With Google Identity Platform the issuer
// is https://securetoken.google.com/PROJECT_ID and the audience is PROJECT_ID;
// any other OIDC provider works by changing those two settings.
type oidcAuth struct{ verifier *oidc.IDTokenVerifier }

// newOIDCAuth fetches the issuer's discovery document. ctx must outlive the
// authenticator, because the verifier refreshes signing keys with it.
func newOIDCAuth(ctx context.Context, issuer, audience string) (*oidcAuth, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	return &oidcAuth{verifier: provider.Verifier(&oidc.Config{ClientID: audience})}, nil
}

func (a *oidcAuth) UserID(r *http.Request) (string, error) {
	raw, ok := bearerToken(r)
	if !ok {
		return "", errNoToken
	}
	tok, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return "", err
	}
	if tok.Subject == "" {
		return "", errors.New("token has no subject")
	}
	return tok.Subject, nil
}

// devAuth attributes every request to one fixed user. It exists so the local
// Compose stack works without a real identity provider, and is only ever
// enabled explicitly (AUTH_DEV_USER).
type devAuth struct{ user string }

func (d devAuth) UserID(*http.Request) (string, error) { return d.user, nil }

// newAuthFromEnv picks the authenticator, failing closed:
//
//	AUTH_ISSUER + AUTH_AUDIENCE  -> verify OIDC ID tokens (production)
//	AUTH_DEV_USER                -> every request is that user (local only)
//	neither                      -> error; the gateway refuses to start
func newAuthFromEnv(ctx context.Context, log *slog.Logger) (authenticator, error) {
	if issuer := config.String("AUTH_ISSUER", ""); issuer != "" {
		audience := config.String("AUTH_AUDIENCE", "")
		if audience == "" {
			return nil, errors.New("AUTH_ISSUER is set but AUTH_AUDIENCE is not")
		}
		return newOIDCAuth(ctx, issuer, audience)
	}
	if user := config.String("AUTH_DEV_USER", ""); user != "" {
		log.Warn("authentication disabled: every upload is attributed to AUTH_DEV_USER",
			"user", user)
		return devAuth{user: user}, nil
	}
	return nil, errors.New("no authentication configured: set AUTH_ISSUER and AUTH_AUDIENCE, " +
		"or AUTH_DEV_USER for local development")
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// stripInternalHeaders removes the internal user header from every inbound
// request, so a client can never choose who it is by sending it itself.
func stripInternalHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(httpx.UserHeader)
		next.ServeHTTP(w, r)
	})
}

// requireUser rejects requests without valid credentials. For the rest it
// replaces the client's token with the verified user ID in httpx.UserHeader,
// so internal services learn who the caller is without ever seeing the token.
func requireUser(auth authenticator, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := auth.UserID(r)
		if err != nil {
			httpx.LoggerFrom(r.Context(), log).Info("rejected unauthenticated request", "err", err)
			w.Header().Set("WWW-Authenticate", `Bearer realm="snapvault"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		r.Header.Del("Authorization")
		r.Header.Set(httpx.UserHeader, user)
		next.ServeHTTP(w, r)
	})
}
