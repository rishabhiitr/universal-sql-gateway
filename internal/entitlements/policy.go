package entitlements

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	Tables map[string]TablePolicy `yaml:"tables"`
}

type TablePolicy struct {
	AllowedRoles []string                  `yaml:"allowed_roles"`
	RowFilters   []RowFilterRule           `yaml:"row_filters"`
	ColumnMasks  map[string]ColumnMaskRule `yaml:"column_masks"`
}

type RowFilterRule struct {
	Role           string `yaml:"role"`
	Column         string `yaml:"column"`
	PrincipalField string `yaml:"principal_field"`
}

type ColumnMaskRule struct {
	ExceptRoles []string `yaml:"except_roles"`
	Mask        string   `yaml:"mask"`
}

func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var policy Policy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, err
	}
	if policy.Tables == nil {
		policy.Tables = make(map[string]TablePolicy)
	}
	return &policy, nil
}
