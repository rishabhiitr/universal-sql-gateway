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
	reservation := limiter.Reserve()
	if !reservation.OK() {
		return qerrors.New(
			qerrors.CodeRateLimitExhausted,
			"connector request budget exhausted",
			connectorID,
			0,
			nil,
		)
	}

	delay := reservation.DelayFrom(time.Now())
	if delay <= 0 {
		return nil
	}

	reservation.Cancel()
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
