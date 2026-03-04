package planner

import "testing"

func TestParseSQLSingleTable(t *testing.T) {
	parser := NewParser()
	plan, err := parser.ParseSQL("SELECT gh.title, gh.state FROM github.pull_requests gh WHERE gh.state = 'open' LIMIT 10")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(plan.Sources) != 1 {
		t.Fatalf("expected one source, got %d", len(plan.Sources))
	}
	if plan.Sources[0].ConnectorID != "github" {
		t.Fatalf("expected github connector, got %s", plan.Sources[0].ConnectorID)
	}
	if plan.Limit != 10 {
		t.Fatalf("expected limit 10, got %d", plan.Limit)
	}
	if len(plan.Sources[0].Filters) != 1 {
		t.Fatalf("expected one pushdown filter, got %d", len(plan.Sources[0].Filters))
	}
}

func TestParseSQLJoinAndOrderBy(t *testing.T) {
	parser := NewParser()
	sql := "SELECT gh.title, j.issue_key FROM github.pull_requests gh JOIN jira.issues j ON gh.jira_issue_id = j.issue_key WHERE gh.state = 'open' ORDER BY gh.title DESC LIMIT 5"
	plan, err := parser.ParseSQL(sql)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if plan.Join == nil {
		t.Fatalf("expected join spec")
	}
	if plan.Join.Left.Alias != "gh" || plan.Join.Right.Alias != "j" {
		t.Fatalf("unexpected aliases in join: left=%s right=%s", plan.Join.Left.Alias, plan.Join.Right.Alias)
	}
	if len(plan.OrderBy) != 1 {
		t.Fatalf("expected one order by, got %d", len(plan.OrderBy))
	}
	if plan.OrderBy[0].Direction != "desc" {
		t.Fatalf("expected desc order, got %s", plan.OrderBy[0].Direction)
	}
}

func TestParseSQLInvalid(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseSQL("DELETE FROM github.pull_requests"); err == nil {
		t.Fatalf("expected error for non-select query")
	}
}
