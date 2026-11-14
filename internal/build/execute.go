package build

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vt100io/molt/internal/compile"
)

// Execute runs a compiled script statement by statement on one connection, so
// the script's transaction spans all of them. On the first error it rolls back
// and returns the error, labelled with the model or test that failed. progress,
// if not nil, is called after each statement succeeds.
func Execute(ctx context.Context, db *sql.DB, script *compile.Script, progress func(compile.Statement, time.Duration)) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to open connection: %w", err)
	}
	defer conn.Close()

	for _, stmt := range script.Statements {
		start := time.Now()
		if _, err := conn.ExecContext(ctx, stmt.SQL); err != nil {
			// Harmless when no transaction is open (an error in init.sql).
			conn.ExecContext(ctx, "ROLLBACK")
			return fmt.Errorf("%s %s: %w", stmt.Kind, stmt.Label, err)
		}
		if progress != nil {
			progress(stmt, time.Since(start))
		}
	}
	return nil
}
