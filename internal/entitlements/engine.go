package entitlements

import (
	"fmt"

	"github.com/rishabhm/universal-sql-query-layer/internal/models"
	qerrors "github.com/rishabhm/universal-sql-query-layer/pkg/errors"
)

type Engine struct {
	policy *Policy
}

func NewEngine(policy *Policy) *Engine {
	return &Engine{policy: policy}
}

func (e *Engine) CheckTableAccess(principal *models.Principal, table string) *qerrors.QueryError {
	tblPolicy, ok := e.policy.Tables[table]
	if !ok {
		return qerrors.New(
			qerrors.CodeEntitlementDenied,
			fmt.Sprintf("access denied for table %q", table),
			table,
			0,
			nil,
		)
	}

	if len(tblPolicy.AllowedRoles) == 0 {
		return nil
	}

	for _, role := range principal.Roles {
		if contains(tblPolicy.AllowedRoles, role) {
			return nil
		}
	}

	return qerrors.New(
		qerrors.CodeEntitlementDenied,
		fmt.Sprintf("principal does not have required role for table %q", table),
		table,
		0,
		nil,
	)
}

func (e *Engine) ApplyRLS(principal *models.Principal, table string, rows []models.Row) []models.Row {
	tblPolicy, ok := e.policy.Tables[table]
	if !ok || len(rows) == 0 || len(tblPolicy.RowFilters) == 0 {
		return rows
	}

	var out []models.Row
	for _, row := range rows {
		if e.allowRow(principal, row, tblPolicy.RowFilters) {
			out = append(out, row)
		}
	}
	return out
}

func (e *Engine) ApplyCLS(principal *models.Principal, table string, rows []models.Row) []models.Row {
	tblPolicy, ok := e.policy.Tables[table]
	if !ok || len(rows) == 0 || len(tblPolicy.ColumnMasks) == 0 {
		return rows
	}

	out := make([]models.Row, 0, len(rows))
	for _, row := range rows {
		cloned := cloneRow(row)
		for column, maskRule := range tblPolicy.ColumnMasks {
			if containsAny(principal.Roles, maskRule.ExceptRoles) {
				continue
			}
			if _, exists := cloned[column]; exists {
				cloned[column] = maskRule.Mask
			}
		}
		out = append(out, cloned)
	}
	return out
}

func (e *Engine) allowRow(principal *models.Principal, row models.Row, rules []RowFilterRule) bool {
	for _, rule := range rules {
		if !contains(principal.Roles, rule.Role) {
			continue
		}

		expected := principalValue(principal, rule.PrincipalField)
		if expected == "" {
			return false
		}
		actual, ok := row[rule.Column]
		if !ok {
			return false
		}
		if fmt.Sprint(actual) != expected {
			return false
		}
	}
	return true
}

func principalValue(principal *models.Principal, field string) string {
	switch field {
	case "user_id":
		return principal.UserID
	case "username":
		return principal.Username
	case "email":
		return principal.Email
	default:
		return ""
	}
}

func cloneRow(in models.Row) models.Row {
	out := make(models.Row, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func contains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func containsAny(have, want []string) bool {
	for _, h := range have {
		if contains(want, h) {
			return true
		}
	}
	return false
}
