package github

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/rishabhm/universal-sql-query-layer/internal/models"
)

const (
	ConnectorID = "github"
	TablePRs    = "github.pull_requests"
)

type Connector struct {
	latency time.Duration
	rows    []models.Row
}

func New(latency time.Duration) *Connector {
	return &Connector{
		latency: latency,
		rows:    seedPRs(200),
	}
}

func (c *Connector) ID() string {
	return ConnectorID
}

func (c *Connector) Tables() []string {
	return []string{TablePRs}
}

func (c *Connector) Schema(table string) ([]models.Column, error) {
	if table != TablePRs {
		return nil, fmt.Errorf("table %q not supported", table)
	}
	return []models.Column{
		{Name: "pr_number", Type: "int", Source: TablePRs},
		{Name: "title", Type: "string", Source: TablePRs},
		{Name: "state", Type: "string", Source: TablePRs},
		{Name: "repo", Type: "string", Source: TablePRs},
		{Name: "jira_issue_id", Type: "string", Source: TablePRs},
		{Name: "author", Type: "string", Source: TablePRs},
		{Name: "email", Type: "string", Source: TablePRs},
		{Name: "created_at", Type: "time", Source: TablePRs},
	}, nil
}

func (c *Connector) Fetch(ctx context.Context, _ *models.Principal, sourceQuery models.SourceQuery) ([]models.Row, models.SourceMeta, error) {
	if sourceQuery.Table != TablePRs {
		return nil, models.SourceMeta{}, fmt.Errorf("table %q not supported", sourceQuery.Table)
	}

	if err := sleepWithContext(ctx, c.latency); err != nil {
		return nil, models.SourceMeta{}, err
	}

	out := make([]models.Row, 0, len(c.rows))
	for _, row := range c.rows {
		if !matchesFilters(row, sourceQuery.Filters) {
			continue
		}
		out = append(out, row)
		if sourceQuery.Limit > 0 && len(out) >= sourceQuery.Limit {
			break
		}
	}

	return out, models.SourceMeta{
		ConnectorID: ConnectorID,
		Table:       TablePRs,
		RowsScanned: len(c.rows),
		FetchedAt:   time.Now().UTC(),
	}, nil
}

func seedPRs(n int) []models.Row {
	rng := rand.New(rand.NewSource(42))
	users := []string{"alice", "bob", "charlie", "dana", "eva"}
	repos := []string{"acme/api", "acme/web", "acme/infra"}
	states := []string{"open", "closed"}

	rows := make([]models.Row, 0, n)
	for i := 0; i < n; i++ {
		author := users[i%len(users)]
		issueID := fmt.Sprintf("PROJ-%d", 100+(i%100))
		createdAt := time.Now().Add(-time.Duration(rng.Intn(720)) * time.Hour).UTC()
		rows = append(rows, models.Row{
			"pr_number":     i + 1,
			"title":         fmt.Sprintf("Improve service behavior #%d", i+1),
			"state":         states[i%len(states)],
			"repo":          repos[i%len(repos)],
			"jira_issue_id": issueID,
			"author":        author,
			"email":         fmt.Sprintf("%s@acme.dev", author),
			"created_at":    createdAt.Format(time.RFC3339),
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
