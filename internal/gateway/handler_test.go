package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"github.com/rishabhm/universal-sql-query-layer/internal/cache"
	"github.com/rishabhm/universal-sql-query-layer/internal/connectors"
	githubconnector "github.com/rishabhm/universal-sql-query-layer/internal/connectors/github"
	jiraconnector "github.com/rishabhm/universal-sql-query-layer/internal/connectors/jira"
	"github.com/rishabhm/universal-sql-query-layer/internal/entitlements"
	"github.com/rishabhm/universal-sql-query-layer/internal/models"
	"github.com/rishabhm/universal-sql-query-layer/internal/planner"
	"github.com/rishabhm/universal-sql-query-layer/internal/ratelimit"
	"github.com/rishabhm/universal-sql-query-layer/pkg/middleware"
)

func TestQuerySuccess(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	reqBody := map[string]any{
		"sql":              "SELECT gh.title, gh.state, j.issue_key, j.status, j.assignee FROM github.pull_requests gh JOIN jira.issues j ON gh.jira_issue_id = j.issue_key WHERE gh.state = 'open' LIMIT 10",
		"max_staleness_ms": 60000,
	}

	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestQueryEntitlementDenied(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	reqBody := map[string]any{
		"sql": "SELECT gh.title FROM github.pull_requests gh LIMIT 1",
	}

	resp := doQuery(t, server, tokenForRoles(t, []string{"guest"}), reqBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestQueryRateLimitExhausted(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 0.01, Burst: 1})
	reqBody := map[string]any{
		"sql":              "SELECT gh.title FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 1",
		"max_staleness_ms": -1,
	}

	first := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", first.StatusCode)
	}

	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected second request 429, got %d", second.StatusCode)
	}
}

func TestQueryCacheHit(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	reqBody := map[string]any{
		"sql":              "SELECT gh.title FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 5",
		"max_staleness_ms": 60000,
	}

	_ = doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)

	var payload map[string]any
	if err := json.NewDecoder(second.Body).Decode(&payload); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	cacheHit, _ := payload["cache_hit"].(bool)
	if !cacheHit {
		t.Fatalf("expected cache_hit=true in response payload")
	}
}

func TestQueryInvalidSQL(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	reqBody := map[string]any{
		"sql": "DELETE FROM github.pull_requests",
	}

	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestQueryMissingAuth(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	body, _ := json.Marshal(map[string]any{
		"sql": "SELECT gh.title FROM github.pull_requests gh LIMIT 1",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Code)
	}
}

func newTestServer(t *testing.T, defaultRate ratelimit.Config) http.Handler {
	t.Helper()

	policy, err := entitlements.LoadPolicy("../../configs/policy.yaml")
	if err != nil {
		t.Fatalf("load policy failed: %v", err)
	}

	cacheStore := cache.New(500 * time.Millisecond)
	t.Cleanup(cacheStore.Stop)

	limiter := ratelimit.New(defaultRate, nil)
	engine := entitlements.NewEngine(policy)
	registry := connectors.NewRegistry(
		githubconnector.New(1*time.Millisecond),
		jiraconnector.New(1*time.Millisecond),
	)
	executor := planner.NewExecutor(registry, engine, limiter, cacheStore, 2*time.Minute, nil)
	handler := NewHandler(planner.NewParser(), executor, zap.NewNop())

	r := chi.NewRouter()
	r.Use(middleware.Auth([]byte(testSecret)))
	r.Post("/v1/query", handler.Query)
	return r
}

func doQuery(t *testing.T, server http.Handler, token string, payload map[string]any) *http.Response {
	t.Helper()

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec.Result()
}

const testSecret = "test-secret"

func tokenForRoles(t *testing.T, roles []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":       "u-1",
		"tenant_id": "t-1",
		"username":  "alice",
		"email":     "alice@acme.dev",
		"roles":     roles,
		"exp":       time.Now().Add(1 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return signed
}

func decodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}
}

func TestQueryFreshnessZeroStalenessForcesFreshFetch(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	sql := `SELECT gh.title FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 5`

	doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              sql,
		"max_staleness_ms": 60000,
	})

	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              sql,
		"max_staleness_ms": 0,
	})
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", second.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, second.Body, &payload)
	if payload.CacheHit {
		t.Fatal("expected cache_hit=false when max_staleness_ms=0")
	}
	if payload.FreshnessMS != 0 {
		t.Fatalf("expected freshness_ms=0 on live fetch, got %d", payload.FreshnessMS)
	}
}

func TestQueryFreshnessNegativeStalenessAlwaysHitsCache(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	sql := `SELECT gh.title FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 5`

	doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              sql,
		"max_staleness_ms": 60000,
	})

	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              sql,
		"max_staleness_ms": -1,
	})
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", second.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, second.Body, &payload)
	if !payload.CacheHit {
		t.Fatal("expected cache_hit=true when max_staleness_ms=-1")
	}
}

func TestQueryFreshnessMSIsZeroOnLiveFetch(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql": `SELECT gh.title FROM github.pull_requests gh LIMIT 1`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if payload.CacheHit {
		t.Fatal("expected cache_hit=false on first (cold) fetch")
	}
	if payload.FreshnessMS != 0 {
		t.Fatalf("expected freshness_ms=0 on live fetch, got %d", payload.FreshnessMS)
	}
}

func TestQueryFreshnessMSNonZeroOnCacheHit(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	sql := `SELECT gh.title FROM github.pull_requests gh LIMIT 1`

	doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              sql,
		"max_staleness_ms": 60000,
	})

	time.Sleep(10 * time.Millisecond)

	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              sql,
		"max_staleness_ms": 60000,
	})
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", second.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, second.Body, &payload)
	if !payload.CacheHit {
		t.Fatal("expected cache_hit=true on second request")
	}
	if payload.FreshnessMS <= 0 {
		t.Fatalf("expected freshness_ms > 0 on cache hit, got %d", payload.FreshnessMS)
	}
}

func TestQueryResponsePayloadFields(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              `SELECT gh.title, gh.state FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 5`,
		"max_staleness_ms": 60000,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if payload.TraceID == "" {
		t.Error("expected non-empty trace_id")
	}
	if len(payload.Columns) == 0 {
		t.Error("expected non-empty columns")
	}
	if len(payload.Rows) == 0 {
		t.Error("expected non-empty rows")
	}
	if len(payload.Sources) == 0 {
		t.Error("expected non-empty sources metadata")
	}
	if payload.FreshnessMS < 0 {
		t.Errorf("freshness_ms must be >= 0, got %d", payload.FreshnessMS)
	}
}

func TestQueryCLSDeveloperEmailMasked(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"developer"}), map[string]any{
		"sql":              `SELECT gh.email FROM github.pull_requests gh LIMIT 5`,
		"max_staleness_ms": 60000,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if len(payload.Rows) == 0 {
		t.Fatal("expected rows in response")
	}
	for _, row := range payload.Rows {
		for key, val := range row {
			if strings.Contains(key, "email") {
				if fmt.Sprint(val) != "[REDACTED]" {
					t.Fatalf("expected email=[REDACTED] for developer, got key=%s val=%v", key, val)
				}
			}
		}
	}
}

func TestQueryCLSAdminEmailNotMasked(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              `SELECT gh.email FROM github.pull_requests gh LIMIT 5`,
		"max_staleness_ms": 60000,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if len(payload.Rows) == 0 {
		t.Fatal("expected rows in response")
	}
	for _, row := range payload.Rows {
		for key, val := range row {
			if strings.Contains(key, "email") {
				if fmt.Sprint(val) == "[REDACTED]" {
					t.Fatalf("expected real email for admin, got [REDACTED] at key=%s", key)
				}
			}
		}
	}
}

func TestQueryRLSDeveloperSeesOnlyOwnRows(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})

	adminResp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql": `SELECT gh.author FROM github.pull_requests gh`,
	})
	var adminPayload models.QueryResponse
	decodeJSON(t, adminResp.Body, &adminPayload)

	devResp := doQuery(t, server, tokenForRoles(t, []string{"developer"}), map[string]any{
		"sql":              `SELECT gh.author FROM github.pull_requests gh`,
		"max_staleness_ms": 0,
	})
	if devResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", devResp.StatusCode)
	}
	var devPayload models.QueryResponse
	decodeJSON(t, devResp.Body, &devPayload)

	if len(devPayload.Rows) >= len(adminPayload.Rows) {
		t.Fatalf("developer should see fewer rows than admin: developer=%d admin=%d",
			len(devPayload.Rows), len(adminPayload.Rows))
	}
	for _, row := range devPayload.Rows {
		for key, val := range row {
			if strings.HasSuffix(key, "author") {
				if fmt.Sprint(val) != "alice" {
					t.Fatalf("RLS violation: developer should only see author=alice, got key=%s val=%v", key, val)
				}
			}
		}
	}
}

func TestQueryRateLimitRetryAfterHeader(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 0.01, Burst: 1})
	reqBody := map[string]any{
		"sql":              `SELECT gh.title FROM github.pull_requests gh LIMIT 1`,
		"max_staleness_ms": -1,
	}
	doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)

	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", second.StatusCode)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429 response")
	}
}

func TestQueryRateLimitErrorBody(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 0.01, Burst: 1})
	reqBody := map[string]any{
		"sql":              `SELECT gh.title FROM github.pull_requests gh LIMIT 1`,
		"max_staleness_ms": -1,
	}
	doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)

	second := doQuery(t, server, tokenForRoles(t, []string{"admin"}), reqBody)
	var errBody map[string]any
	decodeJSON(t, second.Body, &errBody)
	if errBody["code"] != "RATE_LIMIT_EXHAUSTED" {
		t.Fatalf("expected code=RATE_LIMIT_EXHAUSTED, got %v", errBody["code"])
	}
	if fmt.Sprint(errBody["message"]) == "" {
		t.Fatal("expected non-empty message in error body")
	}
	if fmt.Sprint(errBody["trace_id"]) == "" {
		t.Fatal("expected non-empty trace_id in error body")
	}
}

func TestQueryInvalidSQLErrorCode(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql": "DELETE FROM github.pull_requests",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	var errBody map[string]any
	decodeJSON(t, resp.Body, &errBody)
	if errBody["code"] != "INVALID_QUERY" {
		t.Fatalf("expected code=INVALID_QUERY, got %v", errBody["code"])
	}
}

func TestQueryEntitlementDeniedErrorCode(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"guest"}), map[string]any{
		"sql": `SELECT gh.title FROM github.pull_requests gh LIMIT 1`,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	var errBody map[string]any
	decodeJSON(t, resp.Body, &errBody)
	if errBody["code"] != "ENTITLEMENT_DENIED" {
		t.Fatalf("expected code=ENTITLEMENT_DENIED, got %v", errBody["code"])
	}
}

func TestQueryMissingSQL(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql": "",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	var errBody map[string]any
	decodeJSON(t, resp.Body, &errBody)
	if errBody["code"] != "INVALID_QUERY" {
		t.Fatalf("expected code=INVALID_QUERY, got %v", errBody["code"])
	}
}

func TestQuerySingleSourceGitHub(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              `SELECT gh.title, gh.state, gh.author FROM github.pull_requests gh LIMIT 10`,
		"max_staleness_ms": 60000,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if len(payload.Rows) == 0 {
		t.Fatal("expected rows from github source")
	}
	if len(payload.Columns) == 0 {
		t.Fatal("expected columns in response")
	}
	if len(payload.Sources) != 1 {
		t.Fatalf("expected 1 source meta, got %d", len(payload.Sources))
	}
}

func TestQuerySingleSourceJira(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql":              `SELECT j.issue_key, j.summary, j.status FROM jira.issues j LIMIT 10`,
		"max_staleness_ms": 60000,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if len(payload.Rows) == 0 {
		t.Fatal("expected rows from jira source")
	}
	if len(payload.Sources) != 1 {
		t.Fatalf("expected 1 source meta, got %d", len(payload.Sources))
	}
}

func TestQueryCrossAppJoinReturnsMatchedRows(t *testing.T) {
	server := newTestServer(t, ratelimit.Config{RatePerSecond: 100, Burst: 100})
	resp := doQuery(t, server, tokenForRoles(t, []string{"admin"}), map[string]any{
		"sql": `SELECT gh.title, gh.state, j.issue_key, j.summary ` +
			`FROM github.pull_requests gh ` +
			`JOIN jira.issues j ON gh.jira_issue_id = j.issue_key ` +
			`LIMIT 20`,
		"max_staleness_ms": 60000,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload models.QueryResponse
	decodeJSON(t, resp.Body, &payload)
	if len(payload.Rows) == 0 {
		t.Fatal("expected joined rows in response")
	}
	firstRow := payload.Rows[0]
	if _, ok := firstRow["gh.title"]; !ok {
		t.Errorf("expected gh.title in joined row, got keys: %v", firstRow)
	}
	if _, ok := firstRow["j.issue_key"]; !ok {
		t.Errorf("expected j.issue_key in joined row, got keys: %v", firstRow)
	}
	if len(payload.Sources) != 2 {
		t.Fatalf("expected 2 source metas for join query, got %d", len(payload.Sources))
	}
}
