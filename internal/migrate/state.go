package migrate

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// StateStatus reports the shared state database readiness. There is no schema
// version table: readiness means every expected table exists.
type StateStatus struct {
	Backend    string
	Database   any
	Host       any
	Tables     int
	Missing    []string
	SchemaGood bool
}

// StateSchemaStatus inspects the configured state database without creating
// anything.
func StateSchemaStatus(ctx context.Context, settings state.StateSettings) (StateStatus, error) {
	stores, err := state.New(settings, false)
	if err != nil {
		return StateStatus{}, err
	}
	defer func() { _ = stores.Close() }()
	return inspectSchema(ctx, stores)
}

// EnsureStateSchema opens the state database, creating missing tables and
// applying additive migrations (idempotent), then reports the result.
func EnsureStateSchema(ctx context.Context, settings state.StateSettings) (StateStatus, error) {
	stores, err := state.New(settings, true)
	if err != nil {
		return StateStatus{}, err
	}
	defer func() { _ = stores.Close() }()
	return inspectSchema(ctx, stores)
}

func inspectSchema(ctx context.Context, stores *state.Stores) (StateStatus, error) {
	description := stores.Describe()
	status := StateStatus{
		Backend:  fmt.Sprint(description["backend"]),
		Database: description["database"],
		Host:     description["host"],
		Missing:  make([]string, 0),
	}
	for _, table := range state.SchemaTables() {
		query := "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ? LIMIT 1"
		if stores.Dialect() == state.DialectMySQL {
			query = "SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1"
		}
		var found int
		err := stores.DB.QueryRowContext(ctx, query, table.Name).Scan(&found)
		if err == sql.ErrNoRows {
			status.Missing = append(status.Missing, table.Name)
			continue
		}
		if err != nil {
			return StateStatus{}, err
		}
		status.Tables++
	}
	status.SchemaGood = len(status.Missing) == 0
	return status, nil
}
