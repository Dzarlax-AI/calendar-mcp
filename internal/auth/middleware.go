package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

type Options struct {
	APIKey               string
	AllowUnauthenticated bool
}

func Middleware(opts Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.APIKey == "" {
				if opts.AllowUnauthenticated {
					next.ServeHTTP(w, r)
					return
				}
				http.Error(w, "authentication is not configured", http.StatusServiceUnavailable)
				return
			}

			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if provided == "" {
				provided = r.Header.Get("X-API-Key")
			}
			if !equal(provided, opts.APIKey) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func equal(provided, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
