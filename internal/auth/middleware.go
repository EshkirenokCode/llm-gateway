package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

type contextKey struct{}

// WithIdentity stores an identity in the request context.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, identity)
}

// FromContext returns the identity attached by the middleware.
func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}

// ErrorWriter renders an error response in the gateway's wire format. The
// proxy package owns that format, so it is injected here.
type ErrorWriter func(w http.ResponseWriter, status int, kind, message string)

// Middleware rejects requests without a valid API key.
func Middleware(a *Authenticator, logger *slog.Logger, writeErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := FromHeader(r.Header.Get("Authorization"))
			if !ok {
				writeErr(w, http.StatusUnauthorized, "invalid_request_error",
					"missing api key: send it as 'Authorization: Bearer sk_...'")
				return
			}

			identity, err := a.Authenticate(r.Context(), token)
			if err != nil {
				status, message := authFailure(err)
				// The key itself never reaches the log; the prefix is enough to
				// tell which tenant is affected.
				prefix, _ := ParsePrefix(token)
				logger.Info("authentication failed", "reason", err.Error(), "tenant_prefix", prefix)
				writeErr(w, status, "invalid_request_error", message)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
		})
	}
}

func authFailure(err error) (int, string) {
	switch {
	case errors.Is(err, ErrUnknownKey):
		return http.StatusUnauthorized, "invalid api key"
	case errors.Is(err, ErrRevoked):
		return http.StatusUnauthorized, "api key has been revoked"
	case errors.Is(err, ErrTenantDisabled):
		return http.StatusForbidden, "tenant is disabled"
	case errors.Is(err, ErrNoCredentials):
		return http.StatusUnauthorized, "missing api key"
	default:
		// A lookup failure is our problem, not the caller's, and the details
		// stay in the log.
		return http.StatusServiceUnavailable, "cannot verify api key right now"
	}
}
