package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

type verifiedCallerKey struct{}

// VerifiedCaller identifies one accepted server credential, not an end user
// behind a shared Gateway key. Only successful middleware can attach it.
type VerifiedCaller struct {
	key, configuration [32]byte
}

func (c VerifiedCaller) KeyFingerprint() [32]byte           { return c.key }
func (c VerifiedCaller) ConfigurationFingerprint() [32]byte { return c.configuration }

func VerifiedCallerFromContext(ctx context.Context) (VerifiedCaller, bool) {
	caller, ok := ctx.Value(verifiedCallerKey{}).(VerifiedCaller)
	return caller, ok && caller.key != [32]byte{} && caller.configuration != [32]byte{}
}

type Options struct {
	APIKey               string
	LegacyAPIKey         string
	AllowUnauthenticated bool
}

func Middleware(opts Options) func(http.Handler) http.Handler {
	primary := credentialFingerprint(opts.APIKey)
	legacy := credentialFingerprint(opts.LegacyAPIKey)
	configuration := sha256.New()
	_, _ = configuration.Write([]byte("calendar-auth-configuration:v1\x00"))
	_, _ = configuration.Write(primary[:])
	_, _ = configuration.Write(legacy[:])
	if opts.AllowUnauthenticated {
		_, _ = configuration.Write([]byte{1})
	} else {
		_, _ = configuration.Write([]byte{0})
	}
	var configFingerprint [32]byte
	copy(configFingerprint[:], configuration.Sum(nil))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.APIKey == "" {
				if opts.AllowUnauthenticated {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedCallerKey{}, VerifiedCaller{})))
					return
				}
				http.Error(w, "authentication is not configured", http.StatusServiceUnavailable)
				return
			}

			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if provided == "" {
				provided = r.Header.Get("X-API-Key")
			}
			validPrimary := equal(provided, opts.APIKey)
			validLegacy := opts.LegacyAPIKey != "" && equal(provided, opts.LegacyAPIKey)
			if !validPrimary && !validLegacy {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			caller := VerifiedCaller{key: credentialFingerprint(provided), configuration: configFingerprint}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedCallerKey{}, caller)))
		})
	}
}

func credentialFingerprint(key string) [32]byte {
	return sha256.Sum256([]byte("calendar-api-credential:v1\x00" + key))
}

func equal(provided, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
