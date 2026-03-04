package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	var (
		userID   = flag.String("user-id", "u-1", "user identifier (sub claim)")
		tenantID = flag.String("tenant-id", "t-1", "tenant identifier")
		username = flag.String("username", "alice", "username claim")
		email    = flag.String("email", "alice@acme.dev", "email claim")
		roles    = flag.String("roles", "viewer", "comma-separated roles")
		secret   = flag.String("secret", envOrDefault("JWT_SECRET", "dev-secret"), "HMAC secret")
		expiry   = flag.Duration("expiry", time.Hour, "token expiry duration")
	)
	flag.Parse()

	claims := jwt.MapClaims{
		"sub":       *userID,
		"tenant_id": *tenantID,
		"username":  *username,
		"email":     *email,
		"roles":     splitCSV(*roles),
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(*expiry).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(*secret))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to sign token: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(signed)
}

func splitCSV(v string) []string {
	raw := strings.Split(v, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
