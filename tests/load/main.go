// Load test for the query gateway. No external dependencies required — runs with `go run`.
//
// Usage:
//
//	go run tests/load/main.go
//	go run tests/load/main.go -url http://localhost:8080 -duration 60s -max-vus 500
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ── token generation ────────────────────────────────────────────────────────

func makeToken(sub, tenantID, username, email, role, secret string) string {
	claims := jwt.MapClaims{
		"sub":       sub,
		"tenant_id": tenantID,
		"username":  username,
		"email":     email,
		"roles":     []string{role},
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(2 * time.Hour).Unix(),
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	return tok
}

// ── queries ──────────────────────────────────────────────────────────────────

type queryCase struct {
	token string
	body  map[string]any
}

func buildQueries(adminTok, devTok, viewerTok string) []queryCase {
	return []queryCase{
		{
			token: adminTok,
			body: map[string]any{
				"sql":             "SELECT gh.title, gh.state FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 10",
				"max_staleness_ms": 60000,
			},
		},
		{
			token: devTok,
			body: map[string]any{
				"sql":             "SELECT gh.title, j.issue_key, j.status FROM github.pull_requests gh JOIN jira.issues j ON gh.jira_issue_id = j.issue_key WHERE gh.state = 'open' LIMIT 20",
				"max_staleness_ms": 60000,
			},
		},
		{
			token: viewerTok,
			body: map[string]any{
				"sql":             "SELECT j.issue_key, j.status FROM jira.issues j LIMIT 20",
				"max_staleness_ms": 120000,
			},
		},
	}
}

// ── metrics ──────────────────────────────────────────────────────────────────

type metrics struct {
	total       atomic.Int64
	ok          atomic.Int64
	rateLimited atomic.Int64
	errors      atomic.Int64
	cacheHits   atomic.Int64
	mu          sync.Mutex
	latencies   []float64
}

func (m *metrics) record(status int, cacheHit bool, latency time.Duration) {
	m.total.Add(1)
	switch {
	case status == 200:
		m.ok.Add(1)
		if cacheHit {
			m.cacheHits.Add(1)
		}
	case status == 429:
		m.rateLimited.Add(1)
	default:
		m.errors.Add(1)
	}
	m.mu.Lock()
	m.latencies = append(m.latencies, float64(latency.Milliseconds()))
	m.mu.Unlock()
}

func (m *metrics) percentile(p float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latencies) == 0 {
		return 0
	}
	sorted := make([]float64, len(m.latencies))
	copy(sorted, m.latencies)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

// ── worker ───────────────────────────────────────────────────────────────────

func worker(baseURL string, queries []queryCase, client *http.Client, m *metrics, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		q := queries[rand.Intn(len(queries))]
		body, _ := json.Marshal(q.body)

		req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/query", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+q.token)
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := client.Do(req)
		latency := time.Since(start)

		if err != nil {
			m.errors.Add(1)
			m.total.Add(1)
			continue
		}

		var payload struct {
			CacheHit bool `json:"cache_hit"`
		}
		json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()

		m.record(resp.StatusCode, payload.CacheHit, latency)
	}
}

// ── ramp ─────────────────────────────────────────────────────────────────────

// ramp manages a pool of goroutines, growing from startVUs to maxVUs linearly
// over rampDuration, then holding at maxVUs for the remainder.
func ramp(baseURL string, queries []queryCase, client *http.Client, m *metrics,
	startVUs, maxVUs int, rampDuration, totalDuration time.Duration) {

	stop := make(chan struct{})
	var wg sync.WaitGroup

	launch := func(n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				worker(baseURL, queries, client, m, stop)
			}()
		}
	}

	active := startVUs
	launch(active)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	rampEnd := time.Now().Add(rampDuration)
	done := time.After(totalDuration)

	for {
		select {
		case <-done:
			close(stop)
			wg.Wait()
			return
		case t := <-ticker.C:
			if t.Before(rampEnd) {
				progress := float64(time.Until(rampEnd)-rampDuration) / float64(-rampDuration)
				target := startVUs + int(float64(maxVUs-startVUs)*progress)
				if target > active {
					launch(target - active)
					active = target
				}
			} else if active < maxVUs {
				launch(maxVUs - active)
				active = maxVUs
			}
		}
	}
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	baseURL := flag.String("url", "http://localhost:8080", "gateway base URL")
	secret := flag.String("secret", envOrDefault("JWT_SECRET", "dev-secret"), "JWT HMAC secret")
	duration := flag.Duration("duration", 60*time.Second, "total test duration")
	maxVUs := flag.Int("max-vus", 500, "peak virtual users")
	startVUs := flag.Int("start-vus", 25, "starting virtual users")
	flag.Parse()

	// t-load is seeded with relaxed rate-limit overrides in the gateway (500 rps/connector)
	// so the load test measures planner/connector latency, not rate-limit rejections.
	adminTok := makeToken("u-1", "t-load", "alice", "alice@acme.dev", "admin", *secret)
	devTok := makeToken("u-2", "t-load", "bob", "bob@acme.dev", "developer", *secret)
	viewerTok := makeToken("u-3", "t-load", "charlie", "charlie@acme.dev", "viewer", *secret)

	queries := buildQueries(adminTok, devTok, viewerTok)
	m := &metrics{}

	client := &http.Client{Timeout: 5 * time.Second}

	rampDuration := *duration * 2 / 3 // ramp over first 2/3, hold for last 1/3

	fmt.Printf("Load test: %s  duration=%s  VUs %d→%d\n\n", *baseURL, *duration, *startVUs, *maxVUs)

	// Print live progress every 5s
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for range tick.C {
			total := m.total.Load()
			fmt.Printf("  [progress] total=%-6d ok=%-6d rate_limited=%-6d errors=%-6d p95=%.0fms\n",
				total, m.ok.Load(), m.rateLimited.Load(), m.errors.Load(), m.percentile(95))
		}
	}()

	start := time.Now()
	ramp(*baseURL, queries, client, m, *startVUs, *maxVUs, rampDuration, *duration)
	elapsed := time.Since(start)

	total := m.total.Load()
	ok := m.ok.Load()
	rl := m.rateLimited.Load()
	errs := m.errors.Load()
	hits := m.cacheHits.Load()
	qps := float64(total) / elapsed.Seconds()

	p50 := m.percentile(50)
	p95 := m.percentile(95)
	p99 := m.percentile(99)

	pass := p95 < 1500 && float64(errs)/float64(total) < 0.01

	fmt.Printf(`
── Results ──────────────────────────────────────
  Duration:      %.1fs
  Total:         %d  (%.0f QPS)
  200 OK:        %d
  429 Rate-ltd:  %d
  Errors:        %d  (%.2f%%)
  Cache hits:    %d  (%.0f%%)

  Latency (ms):  p50=%.0f  p95=%.0f  p99=%.0f

  Thresholds:
    p95 < 1500ms  → %s
    errors < 1%%  → %s

  PASS: %v
─────────────────────────────────────────────────
`,
		elapsed.Seconds(),
		total, qps,
		ok,
		rl,
		errs, pct(errs, total),
		hits, pct(hits, ok),
		p50, p95, p99,
		threshold(p95 < 1500),
		threshold(float64(errs)/float64(total) < 0.01),
		pass,
	)

	if !pass {
		os.Exit(1)
	}
}

func pct(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func threshold(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
