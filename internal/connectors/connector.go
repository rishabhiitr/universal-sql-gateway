package connectors

import (
	"context"
	"fmt"

	"github.com/rishabhm/universal-sql-query-layer/internal/models"
)

type Connector interface {
	ID() string
	Tables() []string
	Schema(table string) ([]models.Column, error)
	Fetch(ctx context.Context, principal *models.Principal, sourceQuery models.SourceQuery) ([]models.Row, models.SourceMeta, error)
}

type Registry struct {
	connectors map[string]Connector
}

func NewRegistry(all ...Connector) *Registry {
	r := &Registry{connectors: make(map[string]Connector, len(all))}
	for _, c := range all {
		r.connectors[c.ID()] = c
	}
	return r
}

func (r *Registry) Get(connectorID string) (Connector, error) {
	c, ok := r.connectors[connectorID]
	if !ok {
		return nil, fmt.Errorf("connector %q not found", connectorID)
	}
	return c, nil
}
