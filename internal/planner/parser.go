package planner

import (
	"strconv"
	"strings"

	"github.com/blastrain/vitess-sqlparser/sqlparser"
	"github.com/rishabhm/universal-sql-query-layer/internal/models"
	qerrors "github.com/rishabhm/universal-sql-query-layer/pkg/errors"
)

type Parser struct{}

func NewParser() *Parser {
	return &Parser{}
}

func (p *Parser) ParseSQL(sql string) (models.QueryPlan, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return models.QueryPlan{}, qerrors.New(qerrors.CodeInvalidQuery, "failed to parse SQL", "", 0, err)
	}

	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return models.QueryPlan{}, qerrors.New(qerrors.CodeInvalidQuery, "only SELECT statements are supported", "", 0, nil)
	}

	plan := models.QueryPlan{SQL: sql}
	if err := p.parseFrom(sel, &plan); err != nil {
		return models.QueryPlan{}, err
	}

	projections, err := p.parseProjections(sel.SelectExprs)
	if err != nil {
		return models.QueryPlan{}, err
	}
	plan.Projections = projections

	if sel.Where != nil {
		filters, err := flattenAndFilters(sel.Where.Expr)
		if err != nil {
			return models.QueryPlan{}, err
		}
		classifyFilters(filters, &plan)
	}

	orderBy, err := p.parseOrderBy(sel.OrderBy)
	if err != nil {
		return models.QueryPlan{}, err
	}
	plan.OrderBy = orderBy

	if sel.Limit != nil {
		limit, err := parseLimit(sel.Limit)
		if err != nil {
			return models.QueryPlan{}, err
		}
		plan.Limit = limit
		for i := range plan.Sources {
			plan.Sources[i].Limit = limit
		}
	}

	return plan, nil
}

func (p *Parser) parseFrom(sel *sqlparser.Select, plan *models.QueryPlan) error {
	if len(sel.From) == 0 {
		return qerrors.New(qerrors.CodeInvalidQuery, "missing FROM clause", "", 0, nil)
	}

	if len(sel.From) != 1 {
		return qerrors.New(qerrors.CodeInvalidQuery, "only one FROM expression is supported", "", 0, nil)
	}

	switch from := sel.From[0].(type) {
	case *sqlparser.AliasedTableExpr:
		source, err := sourceFromAliasedTable(from)
		if err != nil {
			return err
		}
		plan.Sources = []models.SourceQuery{source}
		return nil
	case *sqlparser.JoinTableExpr:
		leftAliased, ok := from.LeftExpr.(*sqlparser.AliasedTableExpr)
		if !ok {
			return qerrors.New(qerrors.CodeInvalidQuery, "unsupported left side in JOIN", "", 0, nil)
		}
		rightAliased, ok := from.RightExpr.(*sqlparser.AliasedTableExpr)
		if !ok {
			return qerrors.New(qerrors.CodeInvalidQuery, "unsupported right side in JOIN", "", 0, nil)
		}

		left, err := sourceFromAliasedTable(leftAliased)
		if err != nil {
			return err
		}
		right, err := sourceFromAliasedTable(rightAliased)
		if err != nil {
			return err
		}
		plan.Sources = []models.SourceQuery{left, right}

		joinSpec, err := parseJoinSpec(from, left, right)
		if err != nil {
			return err
		}
		plan.Join = joinSpec
		return nil
	default:
		return qerrors.New(qerrors.CodeInvalidQuery, "unsupported FROM expression", "", 0, nil)
	}
}

func sourceFromAliasedTable(expr *sqlparser.AliasedTableExpr) (models.SourceQuery, error) {
	tableName, ok := expr.Expr.(sqlparser.TableName)
	if !ok {
		return models.SourceQuery{}, qerrors.New(qerrors.CodeInvalidQuery, "unsupported table expression", "", 0, nil)
	}

	connector := tableName.Qualifier.String()
	table := connector + "." + tableName.Name.String()
	if connector == "" {
		return models.SourceQuery{}, qerrors.New(qerrors.CodeInvalidQuery, "table must be qualified as connector.table", "", 0, nil)
	}

	alias := expr.As.String()
	if alias == "" {
		alias = tableName.Name.String()
	}
	return models.SourceQuery{
		ConnectorID: connector,
		Table:       table,
		Alias:       alias,
	}, nil
}

func parseJoinSpec(join *sqlparser.JoinTableExpr, left, right models.SourceQuery) (*models.JoinSpec, error) {
	comp, ok := join.On.(*sqlparser.ComparisonExpr)
	if !ok || comp.Operator != sqlparser.EqualStr {
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "only equality JOIN conditions are supported", "", 0, nil)
	}

	leftCol, ok := comp.Left.(*sqlparser.ColName)
	if !ok {
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "invalid left JOIN column", "", 0, nil)
	}
	rightCol, ok := comp.Right.(*sqlparser.ColName)
	if !ok {
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "invalid right JOIN column", "", 0, nil)
	}

	leftAlias := leftCol.Qualifier.Name.String()
	rightAlias := rightCol.Qualifier.Name.String()
	if leftAlias == "" || rightAlias == "" {
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "JOIN columns must be fully qualified with aliases", "", 0, nil)
	}

	if leftAlias != left.Alias && rightAlias == left.Alias {
		leftCol, rightCol = rightCol, leftCol
		leftAlias, rightAlias = rightAlias, leftAlias
	}
	if leftAlias != left.Alias || rightAlias != right.Alias {
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "JOIN aliases must match FROM aliases", "", 0, nil)
	}

	return &models.JoinSpec{
		Type: "inner",
		Left: models.SourceRef{
			ConnectorID: left.ConnectorID,
			Table:       left.Table,
			Alias:       left.Alias,
		},
		Right: models.SourceRef{
			ConnectorID: right.ConnectorID,
			Table:       right.Table,
			Alias:       right.Alias,
		},
		LeftKey:  leftCol.Name.String(),
		RightKey: rightCol.Name.String(),
	}, nil
}

func (p *Parser) parseProjections(in sqlparser.SelectExprs) ([]models.ColumnRef, error) {
	out := make([]models.ColumnRef, 0, len(in))
	for _, expr := range in {
		switch e := expr.(type) {
		case *sqlparser.AliasedExpr:
			col, ok := e.Expr.(*sqlparser.ColName)
			if !ok {
				return nil, qerrors.New(qerrors.CodeInvalidQuery, "only column projections are supported", "", 0, nil)
			}
			out = append(out, models.ColumnRef{
				SourceAlias: col.Qualifier.Name.String(),
				Column:      col.Name.String(),
				As:          e.As.String(),
			})
		case *sqlparser.StarExpr:
			out = append(out, models.ColumnRef{SourceAlias: e.TableName.Name.String(), Column: "*"})
		default:
			return nil, qerrors.New(qerrors.CodeInvalidQuery, "unsupported projection expression", "", 0, nil)
		}
	}
	return out, nil
}

func (p *Parser) parseOrderBy(in sqlparser.OrderBy) ([]models.OrderBySpec, error) {
	out := make([]models.OrderBySpec, 0, len(in))
	for _, order := range in {
		col, ok := order.Expr.(*sqlparser.ColName)
		if !ok {
			return nil, qerrors.New(qerrors.CodeInvalidQuery, "ORDER BY supports columns only", "", 0, nil)
		}
		direction := strings.ToLower(order.Direction)
		if direction == "" {
			direction = "asc"
		}
		out = append(out, models.OrderBySpec{
			SourceAlias: col.Qualifier.Name.String(),
			Column:      col.Name.String(),
			Direction:   direction,
		})
	}
	return out, nil
}

func parseLimit(limit *sqlparser.Limit) (int, error) {
	if limit.Rowcount == nil {
		return 0, nil
	}
	val, ok := limit.Rowcount.(*sqlparser.SQLVal)
	if !ok {
		return 0, qerrors.New(qerrors.CodeInvalidQuery, "LIMIT must be a literal integer", "", 0, nil)
	}
	n, err := strconv.Atoi(string(val.Val))
	if err != nil {
		return 0, qerrors.New(qerrors.CodeInvalidQuery, "LIMIT must be a literal integer", "", 0, err)
	}
	return n, nil
}

func flattenAndFilters(expr sqlparser.Expr) ([]models.FilterExpr, error) {
	switch e := expr.(type) {
	case *sqlparser.AndExpr:
		left, err := flattenAndFilters(e.Left)
		if err != nil {
			return nil, err
		}
		right, err := flattenAndFilters(e.Right)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	case *sqlparser.ComparisonExpr:
		leftCol, ok := e.Left.(*sqlparser.ColName)
		if !ok {
			return nil, qerrors.New(qerrors.CodeInvalidQuery, "left side of comparison must be a column", "", 0, nil)
		}
		rightVal, err := literalFromExpr(e.Right)
		if err != nil {
			return nil, err
		}
		return []models.FilterExpr{
			{
				Left: models.OperandRef{
					SourceAlias: leftCol.Qualifier.Name.String(),
					Column:      leftCol.Name.String(),
				},
				Op:    e.Operator,
				Right: rightVal,
			},
		}, nil
	default:
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "unsupported WHERE expression", "", 0, nil)
	}
}

func literalFromExpr(expr sqlparser.Expr) (any, error) {
	switch v := expr.(type) {
	case *sqlparser.SQLVal:
		switch v.Type {
		case sqlparser.IntVal:
			n, err := strconv.Atoi(string(v.Val))
			if err != nil {
				return nil, qerrors.New(qerrors.CodeInvalidQuery, "invalid integer literal", "", 0, err)
			}
			return n, nil
		default:
			return string(v.Val), nil
		}
	case sqlparser.BoolVal:
		return bool(v), nil
	default:
		return nil, qerrors.New(qerrors.CodeInvalidQuery, "right side of comparison must be a literal", "", 0, nil)
	}
}

func classifyFilters(filters []models.FilterExpr, plan *models.QueryPlan) {
	aliasIndex := make(map[string]int, len(plan.Sources))
	for i, source := range plan.Sources {
		aliasIndex[source.Alias] = i
	}

	for _, f := range filters {
		if len(plan.Sources) == 1 {
			plan.Sources[0].Filters = append(plan.Sources[0].Filters, f)
			continue
		}

		if idx, ok := aliasIndex[f.Left.SourceAlias]; ok && f.Left.SourceAlias != "" {
			plan.Sources[idx].Filters = append(plan.Sources[idx].Filters, f)
			continue
		}
		plan.PostFilters = append(plan.PostFilters, f)
	}
}
