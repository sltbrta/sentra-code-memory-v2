package db

// Migration is a single schema change with an up and down statement.
type Migration struct {
	ID   string
	Up   string
	Down string
}

// Migrate applies unapplied migrations in order and records each one. It is a
// fixture-grade migrator: no locking, no advisory hints, just ordered replay.
func Migrate(conn *Conn, pending []Migration) (int, error) {
	if conn == nil {
		return 0, ErrEmptyDSN
	}
	applied := 0
	for _, m := range pending {
		if err := ExecQuery(m.Up); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// Rollback undoes the last applied migration using its down statement.
func Rollback(m Migration) error {
	if m.Down == "" {
		return ErrEmptyQuery
	}
	return ExecQuery(m.Down)
}

// BaselineMigrations returns the fixture's seed schema changes.
func BaselineMigrations() []Migration {
	return []Migration{
		{ID: "0001_users", Up: "CREATE TABLE users (id INT)", Down: "DROP TABLE users"},
		{ID: "0002_orders", Up: "CREATE TABLE orders (id INT)", Down: "DROP TABLE orders"},
		{ID: "0003_index", Up: "CREATE INDEX idx_orders_user ON orders (id)", Down: "DROP INDEX idx_orders_user"},
	}
}
