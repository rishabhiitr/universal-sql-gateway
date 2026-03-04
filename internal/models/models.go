package models

import "time"

type Principal struct {
	UserID    string   `json:"user_id"`
	TenantID  string   `json:"tenant_id"`
	Username  string   `json:"username"`
	Email     string   `json:"email,omitempty"`
	Roles     []string `json:"roles"`
	Scopes    []string `json:"scopes,omitempty"`
	TokenID   string   `json:"token_id,omitempty"`
	IssuedAt  int64    `json:"issued_at,omitempty"`
	ExpiresAt int64    `json:"expires_at,omitempty"`
}

type QueryRequest struct {
	SQL            string `json:"sql"`
	MaxStalenessMS int64  `json:"max_staleness_ms,omitempty"`
	PageSize       int    `json:"page_size,omitempty"`
	PageToken      string `json:"page_token,omitempty"`
}

type QueryResponse struct {
	Columns         []Column          `json:"columns"`
	Rows            []Row             `json:"rows"`
	NextPageToken   string            `json:"next_page_token,omitempty"`
	FreshnessMS     int64             `json:"freshness_ms"`
	CacheHit        bool              `json:"cache_hit"`
	TraceID         string            `json:"trace_id"`
	RateLimitStatus []RateLimitStatus `json:"rate_limit_status,omitempty"`
	Sources         []SourceMeta      `json:"sources,omitempty"`
}

type QueryPlan struct {
	SQL            string        `json:"sql"`
	Projections    []ColumnRef   `json:"projections"`
	Sources        []SourceQuery `json:"sources"`
	Join           *JoinSpec     `json:"join,omitempty"`
	PostFilters    []FilterExpr  `json:"post_filters,omitempty"`
	OrderBy        []OrderBySpec `json:"order_by,omitempty"`
	Limit          int           `json:"limit,omitempty"`
	MaxStalenessMS int64         `json:"max_staleness_ms,omitempty"`
}

type SourceQuery struct {
	ConnectorID string       `json:"connector_id"`
	Table       string       `json:"table"`
	Alias       string       `json:"alias,omitempty"`
	Columns     []string     `json:"columns,omitempty"`
	Filters     []FilterExpr `json:"filters,omitempty"`
	Limit       int          `json:"limit,omitempty"`
}

type JoinSpec struct {
	Type     string    `json:"type"` // inner, left
	Left     SourceRef `json:"left"`
	Right    SourceRef `json:"right"`
	LeftKey  string    `json:"left_key"`
	RightKey string    `json:"right_key"`
}

type FilterExpr struct {
	Left  OperandRef `json:"left"`
	Op    string     `json:"op"` // =, !=, >, >=, <, <=
	Right any        `json:"right"`
}

type OperandRef struct {
	SourceAlias string `json:"source_alias,omitempty"`
	Column      string `json:"column"`
}

type SourceRef struct {
	ConnectorID string `json:"connector_id"`
	Table       string `json:"table"`
	Alias       string `json:"alias,omitempty"`
}

type ColumnRef struct {
	SourceAlias string `json:"source_alias,omitempty"`
	Column      string `json:"column"`
	As          string `json:"as,omitempty"`
}

type OrderBySpec struct {
	SourceAlias string `json:"source_alias,omitempty"`
	Column      string `json:"column"`
	Direction   string `json:"direction"` // asc, desc
}

type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Source     string `json:"source,omitempty"`
	IsNullable bool   `json:"is_nullable"`
}

type Row map[string]any

type RateLimitStatus struct {
	ConnectorID string `json:"connector_id"`
	Allowed     bool   `json:"allowed"`
	Remaining   int    `json:"remaining,omitempty"`
	RetryAfterS int64  `json:"retry_after_s,omitempty"`
}

type SourceMeta struct {
	ConnectorID string    `json:"connector_id"`
	Table       string    `json:"table"`
	RowsScanned int       `json:"rows_scanned"`
	FetchedAt   time.Time `json:"fetched_at"`
	FreshnessMS int64     `json:"freshness_ms"`
	CacheHit    bool      `json:"cache_hit"`
}
