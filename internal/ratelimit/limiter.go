package ratelimit

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	qerrors "github.com/rishabhm/universal-sql-query-layer/pkg/errors"
	"golang.org/x/time/rate"
)

type Config struct {
	RatePerSecond float64
	Burst         int
}

type Service struct {
	mu              sync.RWMutex
	buckets         map[string]*rate.Limiter
	configs         map[string]Config
	tenantOverrides map[string]map[string]Config // tenantID → connectorID → Config
	defaultCfg      Config
}

func New(defaultCfg Config, connectorCfg map[string]Config, tenantOverrides map[string]map[string]Config) *Service {
	return &Service{
		buckets:         make(map[string]*rate.Limiter),
		configs:         connectorCfg,
		tenantOverrides: tenantOverrides,
		defaultCfg:      defaultCfg,
	}
}

func (s *Service) Allow(_ context.Context, tenantID, connectorID string) *qerrors.QueryError {
	limiter := s.getOrCreateLimiter(tenantID, connectorID)
	if limiter.Allow() {
		return nil
	}
	// Compute delay without consuming or scheduling any token.
	// tokens < 0 means we are in deficit; time to recover 1 token = 1/rate.
	// tokens >= 0 but Allow() failed means burst is 0; delay = 1/rate.
	tokens := limiter.Tokens()
	var delay time.Duration
	if r := float64(limiter.Limit()); r > 0 {
		deficit := 1.0 - tokens // how many tokens we need beyond current balance
		if deficit < 1.0 {
			deficit = 1.0
		}
		delay = time.Duration(deficit / r * float64(time.Second))
	}
	return qerrors.New(
		qerrors.CodeRateLimitExhausted,
		"connector request budget exhausted; retry later",
		connectorID,
		delay,
		nil,
	)
}

func RetryAfterSeconds(delay time.Duration) int64 {
	if delay <= 0 {
		return 0
	}
	return int64(math.Ceil(delay.Seconds()))
}

func (s *Service) getOrCreateLimiter(tenantID, connectorID string) *rate.Limiter {
	key := fmt.Sprintf("%s:%s", tenantID, connectorID)

	s.mu.RLock()
	limiter, ok := s.buckets[key]
	s.mu.RUnlock()
	if ok {
		return limiter
	}

	cfg := s.defaultCfg
	if override, found := s.configs[connectorID]; found {
		cfg = override
	}
	// Per-tenant overrides take precedence over connector defaults.
	if tenantCfg, ok := s.tenantOverrides[tenantID]; ok {
		if override, found := tenantCfg[connectorID]; found {
			cfg = override
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if limiter, ok = s.buckets[key]; ok {
		return limiter
	}

	limiter = rate.NewLimiter(rate.Limit(cfg.RatePerSecond), cfg.Burst)
	s.buckets[key] = limiter
	return limiter
}
