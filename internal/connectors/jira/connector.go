package jira

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/rishabhm/universal-sql-query-layer/internal/models"
)

const (
	ConnectorID = "jira"
	TableIssues = "jira.issues"
)

type Connector struct {
	latency time.Duration
	rows    []models.Row
}

func New(latency time.Duration) *Connector {
	return &Connector{
		latency: latency,
		rows:    seedIssues(200),
	}
}

func (c *Connector) ID() string {
	return ConnectorID
}

func (c *Connector) Tables() []string {
	return []string{TableIssues}
}

func (c *Connector) Schema(table string) ([]models.Column, error) {
	if table != TableIssues {
		return nil, fmt.Errorf("table %q not supported", table)
	}
	return []models.Column{
		{Name: "issue_key", Type: "string", Source: TableIssues},
		{Name: "summary", Type: "string", Source: TableIssues},
		{Name: "status", Type: "string", Source: TableIssues},
		{Name: "assignee", Type: "string", Source: TableIssues},
		{Name: "assignee_email", Type: "string", Source: TableIssues},
		{Name: "priority", Type: "string", Source: TableIssues},
		{Name: "updated_at", Type: "time", Source: TableIssues},
	}, nil
}

func (c *Connector) Fetch(ctx context.Context, _ *models.Principal, sourceQuery models.SourceQuery) ([]models.Row, models.SourceMeta, error) {
	if sourceQuery.Table != TableIssues {
		return nil, models.SourceMeta{}, fmt.Errorf("table %q not supported", sourceQuery.Table)
	}

	if err := sleepWithContext(ctx, c.latency); err != nil {
		return nil, models.SourceMeta{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := make([]models.Row, 0, len(c.rows))
	for _, row := range c.rows {
		if !matchesFilters(row, sourceQuery.Filters) {
			continue
		}
		copied := make(models.Row, len(row)+1)
		for k, v := range row {
			copied[k] = v
		}
		copied["_fetched_at"] = now
		out = append(out, copied)
		if sourceQuery.Limit > 0 && len(out) >= sourceQuery.Limit {
			break
		}
	}

	return out, models.SourceMeta{
		ConnectorID: ConnectorID,
		Table:       TableIssues,
		RowsScanned: len(c.rows),
		FetchedAt:   time.Now().UTC(),
	}, nil
}

func seedIssues(n int) []models.Row {
	rng := rand.New(rand.NewSource(99))
	users := []string{"alice", "bob", "charlie", "dana", "eva"}
	statuses := []string{"todo", "in_progress", "done"}
	priorities := []string{"P1", "P2", "P3"}

	rows := make([]models.Row, 0, n)
	for i := 0; i < n; i++ {
		assignee := users[i%len(users)]
		issueKey := fmt.Sprintf("PROJ-%d", 100+(i%100))
		updatedAt := time.Now().Add(-time.Duration(rng.Intn(360)) * time.Hour).UTC()
		rows = append(rows, models.Row{
			"issue_key":      issueKey,
			"summary":        fmt.Sprintf("Track integration task #%d", i+1),
			"status":         statuses[i%len(statuses)],
			"assignee":       assignee,
			"assignee_email": fmt.Sprintf("%s@acme.dev", assignee),
			"priority":       priorities[i%len(priorities)],
			"updated_at":     updatedAt.Format(time.RFC3339),
		})
	}
	return rows
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func matchesFilters(row models.Row, filters []models.FilterExpr) bool {
	for _, f := range filters {
		if f.Op != "=" {
			continue
		}
		val, ok := row[f.Left.Column]
		if !ok {
			return false
		}
		if fmt.Sprint(val) != fmt.Sprint(f.Right) {
			return false
		}
	}
	return true
}
