package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/rishabhm/universal-sql-query-layer/internal/cache"
	"github.com/rishabhm/universal-sql-query-layer/internal/connectors"
	"github.com/rishabhm/universal-sql-query-layer/internal/entitlements"
	"github.com/rishabhm/universal-sql-query-layer/internal/models"
	qerrors "github.com/rishabhm/universal-sql-query-layer/pkg/errors"
)

type Executor struct {
	registry     *connectors.Registry
	entitlements *entitlements.Engine
	limiter      limiter
	cache        *cache.TTLCache
	cacheTTL     time.Duration
	tracer       trace.Tracer
}

type limiter interface {
	Allow(ctx context.Context, tenantID, connectorID string) *qerrors.QueryError
}

func NewExecutor(
	registry *connectors.Registry,
	entitlementEngine *entitlements.Engine,
	limiter limiter,
	cacheStore *cache.TTLCache,
	cacheTTL time.Duration,
	tracer trace.Tracer,
) *Executor {
	return &Executor{
		registry:     registry,
		entitlements: entitlementEngine,
		limiter:      limiter,
		cache:        cacheStore,
		cacheTTL:     cacheTTL,
		tracer:       tracer,
	}
}

func (e *Executor) Execute(ctx context.Context, principal *models.Principal, plan models.QueryPlan, req models.QueryRequest) (models.QueryResponse, error) {
	for _, source := range plan.Sources {
		if err := e.entitlements.CheckTableAccess(principal, source.Table); err != nil {
			return models.QueryResponse{}, err
		}
	}

	sourceRows, sourceMetas, rateStatus, err := e.fetchConcurrent(ctx, principal, plan, req)
	if err != nil {
		return models.QueryResponse{}, err
	}

	for _, source := range plan.Sources {
		rows := sourceRows[source.Alias]
		rows = e.entitlements.ApplyRLS(principal, source.Table, rows)
		rows = e.entitlements.ApplyCLS(principal, source.Table, rows)
		sourceRows[source.Alias] = rows
	}

	joinedRows := materializeRows(plan, sourceRows)
	joinedRows = applyPostFilters(joinedRows, plan.PostFilters)
	projectedRows := projectRows(joinedRows, plan.Projections)
	applyOrdering(projectedRows, plan.OrderBy)
	projectedRows = applyLimit(projectedRows, plan.Limit)

	freshnessMS := int64(0)
	cacheHit := true
	for _, meta := range sourceMetas {
		if meta.FreshnessMS > freshnessMS {
			freshnessMS = meta.FreshnessMS
		}
		if !meta.CacheHit {
			cacheHit = false
		}
	}

	return models.QueryResponse{
		Columns:         inferColumns(projectedRows),
		Rows:            projectedRows,
		FreshnessMS:     freshnessMS,
		CacheHit:        cacheHit,
		RateLimitStatus: rateStatus,
		Sources:         sourceMetas,
	}, nil
}

func (e *Executor) fetchConcurrent(
	ctx context.Context,
	principal *models.Principal,
	plan models.QueryPlan,
	req models.QueryRequest,
) (map[string][]models.Row, []models.SourceMeta, []models.RateLimitStatus, error) {
	rowsByAlias := make(map[string][]models.Row, len(plan.Sources))
	metas := make([]models.SourceMeta, len(plan.Sources))
	rateStatuses := make([]models.RateLimitStatus, 0, len(plan.Sources))
	type result struct {
		idx        int
		alias      string
		rows       []models.Row
		meta       models.SourceMeta
		rateStatus models.RateLimitStatus
	}

	outCh := make(chan result, len(plan.Sources))
	eg, egCtx := errgroup.WithContext(ctx)

	for i, source := range plan.Sources {
		i := i
		source := source
		eg.Go(func() error {
			status := models.RateLimitStatus{
				ConnectorID: source.ConnectorID,
				Allowed:     true,
			}

			if rateErr := e.limiter.Allow(egCtx, principal.TenantID, source.ConnectorID); rateErr != nil {
				status.Allowed = false
				status.RetryAfterS = int64(rateErr.RetryAfter.Seconds())
				outCh <- result{idx: i, alias: source.Alias, rateStatus: status}
				return rateErr
			}

			cacheKey := buildCacheKey(principal.TenantID, source)
			maxStaleness := time.Duration(req.MaxStalenessMS) * time.Millisecond
			if cached, staleness, ok := e.cache.Get(cacheKey, maxStaleness); ok {
				rows := cloneRows(cached.([]models.Row))
				outCh <- result{
					idx:   i,
					alias: source.Alias,
					rows:  rows,
					meta: models.SourceMeta{
						ConnectorID: source.ConnectorID,
						Table:       source.Table,
						RowsScanned: len(rows),
						FetchedAt:   time.Now().Add(-staleness),
						FreshnessMS: staleness.Milliseconds(),
						CacheHit:    true,
					},
					rateStatus: status,
				}
				return nil
			}

			connector, err := e.registry.Get(source.ConnectorID)
			if err != nil {
				return err
			}

			var spanCtx context.Context
			var span trace.Span
			if e.tracer != nil {
				spanCtx, span = e.tracer.Start(egCtx, "connector.fetch",
					trace.WithAttributes(
						attribute.String("connector.id", source.ConnectorID),
						attribute.String("connector.table", source.Table),
						attribute.String("tenant.id", principal.TenantID),
					),
				)
				defer span.End()
			} else {
				spanCtx = egCtx
			}

			rows, meta, err := connector.Fetch(spanCtx, principal, source)
			if err != nil {
				if span != nil {
					span.RecordError(err)
				}
				if egCtx.Err() == context.DeadlineExceeded || strings.Contains(err.Error(), "deadline") {
					return qerrors.New(qerrors.CodeSourceTimeout, "source timed out", source.ConnectorID, 0, err)
				}
				return err
			}
			if span != nil {
				span.SetAttributes(attribute.Int("connector.rows_fetched", len(rows)))
			}

			e.cache.Set(cacheKey, cloneRows(rows), e.cacheTTL)
			meta.CacheHit = false
			meta.FreshnessMS = 0
			outCh <- result{
				idx:        i,
				alias:      source.Alias,
				rows:       rows,
				meta:       meta,
				rateStatus: status,
			}
			return nil
		})
	}

	err := eg.Wait()
	close(outCh)
	for r := range outCh {
		if len(r.rows) > 0 {
			rowsByAlias[r.alias] = r.rows
		}
		metas[r.idx] = r.meta
		rateStatuses = append(rateStatuses, r.rateStatus)
	}
	if err != nil {
		return nil, nil, nil, err
	}

	return rowsByAlias, metas, rateStatuses, nil
}

func buildCacheKey(tenantID string, source models.SourceQuery) string {
	raw, _ := json.Marshal(struct {
		TenantID string             `json:"tenant_id"`
		Source   models.SourceQuery `json:"source"`
	}{
		TenantID: tenantID,
		Source:   source,
	})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func materializeRows(plan models.QueryPlan, rowsByAlias map[string][]models.Row) []models.Row {
	if plan.Join == nil {
		source := plan.Sources[0]
		return prefixRows(rowsByAlias[source.Alias], source.Alias)
	}

	leftRows := prefixRows(rowsByAlias[plan.Join.Left.Alias], plan.Join.Left.Alias)
	rightRows := prefixRows(rowsByAlias[plan.Join.Right.Alias], plan.Join.Right.Alias)
	return hashJoin(leftRows, rightRows, plan.Join.Left.Alias+"."+plan.Join.LeftKey, plan.Join.Right.Alias+"."+plan.Join.RightKey)
}

func hashJoin(leftRows, rightRows []models.Row, leftKey, rightKey string) []models.Row {
	if len(leftRows) > len(rightRows) {
		rightRows, leftRows = leftRows, rightRows
		rightKey, leftKey = leftKey, rightKey
	}

	bucket := make(map[string][]models.Row, len(leftRows))
	for _, row := range leftRows {
		key := fmt.Sprint(row[leftKey])
		bucket[key] = append(bucket[key], row)
	}

	out := make([]models.Row, 0, len(rightRows))
	for _, right := range rightRows {
		key := fmt.Sprint(right[rightKey])
		matches := bucket[key]
		for _, left := range matches {
			merged := make(models.Row, len(left)+len(right))
			for k, v := range left {
				merged[k] = v
			}
			for k, v := range right {
				merged[k] = v
			}
			out = append(out, merged)
		}
	}
	return out
}

func prefixRows(rows []models.Row, alias string) []models.Row {
	out := make([]models.Row, 0, len(rows))
	for _, row := range rows {
		item := make(models.Row, len(row))
		for k, v := range row {
			item[alias+"."+k] = v
		}
		out = append(out, item)
	}
	return out
}

func applyPostFilters(rows []models.Row, filters []models.FilterExpr) []models.Row {
	if len(filters) == 0 {
		return rows
	}
	out := make([]models.Row, 0, len(rows))
	for _, row := range rows {
		if matchRow(row, filters) {
			out = append(out, row)
		}
	}
	return out
}

func matchRow(row models.Row, filters []models.FilterExpr) bool {
	for _, f := range filters {
		key := f.Left.Column
		if f.Left.SourceAlias != "" {
			key = f.Left.SourceAlias + "." + f.Left.Column
		}
		val, ok := row[key]
		if !ok && f.Left.SourceAlias == "" {
			// rows are prefixed with alias; scan for suffix match
			suffix := "." + f.Left.Column
			for k, v := range row {
				if strings.HasSuffix(k, suffix) {
					val, ok = v, true
					break
				}
			}
		}
		if !ok {
			return false
		}
		if fmt.Sprint(val) != fmt.Sprint(f.Right) {
			return false
		}
	}
	return true
}

func projectRows(rows []models.Row, projections []models.ColumnRef) []models.Row {
	if len(projections) == 0 {
		return rows
	}
	out := make([]models.Row, 0, len(rows))
	for _, row := range rows {
		projected := make(models.Row)
		for _, col := range projections {
			var key string
			switch {
			case col.Column == "*" && col.SourceAlias != "":
				prefix := col.SourceAlias + "."
				for k, v := range row {
					if strings.HasPrefix(k, prefix) {
						projected[k] = v
					}
				}
				continue
			case col.Column == "*":
				for k, v := range row {
					projected[k] = v
				}
				continue
			case col.SourceAlias != "":
				key = col.SourceAlias + "." + col.Column
			default:
				key = col.Column
			}

			val, ok := row[key]
			if !ok && col.SourceAlias == "" {
				// rows are prefixed with alias; scan for suffix match
				suffix := "." + col.Column
				for k, v := range row {
					if strings.HasSuffix(k, suffix) {
						val, ok = v, true
						key = k
						break
					}
				}
			}
			if ok {
				outKey := col.Column
				if col.SourceAlias != "" {
					outKey = key
				}
				if col.As != "" {
					outKey = col.As
				}
				projected[outKey] = val
			}
		}
		out = append(out, projected)
	}
	return out
}

func applyOrdering(rows []models.Row, orderBy []models.OrderBySpec) {
	if len(orderBy) == 0 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left := rows[i]
		right := rows[j]
		for _, ord := range orderBy {
			key := ord.Column
			if ord.SourceAlias != "" {
				key = ord.SourceAlias + "." + ord.Column
			}
			lv := fmt.Sprint(left[key])
			rv := fmt.Sprint(right[key])
			if lv == rv {
				continue
			}
			if strings.EqualFold(ord.Direction, "desc") {
				return lv > rv
			}
			return lv < rv
		}
		return true
	})
}

func applyLimit(rows []models.Row, limit int) []models.Row {
	if limit <= 0 || len(rows) <= limit {
		return rows
	}
	return rows[:limit]
}

func inferColumns(rows []models.Row) []models.Column {
	if len(rows) == 0 {
		return nil
	}
	cols := make([]models.Column, 0, len(rows[0]))
	for key := range rows[0] {
		cols = append(cols, models.Column{Name: key, Type: "string"})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	return cols
}

func cloneRows(in []models.Row) []models.Row {
	out := make([]models.Row, 0, len(in))
	for _, row := range in {
		cloned := make(models.Row, len(row))
		for k, v := range row {
			cloned[k] = v
		}
		out = append(out, cloned)
	}
	return out
}
