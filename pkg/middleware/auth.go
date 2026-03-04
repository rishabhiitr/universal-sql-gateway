package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rishabhm/universal-sql-query-layer/internal/models"
)

type contextKey int

const principalKey contextKey = iota

func PrincipalFromContext(ctx context.Context) (*models.Principal, bool) {
	p, ok := ctx.Value(principalKey).(*models.Principal)
	return p, ok
}

func Auth(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}

			tokenString := strings.TrimPrefix(authHeader, "Bearer ")
			claims := jwt.MapClaims{}
			token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, errors.New("unexpected signing method")
				}
				return secret, nil
			})
			if err != nil || !token.Valid {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			principal := &models.Principal{
				UserID:   getStringClaim(claims, "sub"),
				TenantID: getStringClaim(claims, "tenant_id"),
				Username: getStringClaim(claims, "username"),
				Email:    getStringClaim(claims, "email"),
				Roles:    getStringSliceClaim(claims, "roles"),
				Scopes:   getStringSliceClaim(claims, "scopes"),
			}
			if principal.TenantID == "" || principal.UserID == "" {
				http.Error(w, "invalid token claims", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), principalKey, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func getStringClaim(claims jwt.MapClaims, key string) string {
	raw, ok := claims[key]
	if !ok {
		return ""
	}
	if str, ok := raw.(string); ok {
		return str
	}
	return ""
}

func getStringSliceClaim(claims jwt.MapClaims, key string) []string {
	raw, ok := claims[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if str, ok := item.(string); ok {
			out = append(out, str)
		}
	}
	return out
}
