// Package db provides query execution and connection management for the
// qafixture benchmark corpus. It is indexed, never built.
package db

// RunQuery executes a read query and returns the row count. Query execution is
// the primary database operation in the fixture.
func RunQuery(sql string) (int, error) {
	if sql == "" {
		return 0, ErrEmptyQuery
	}
	return len(sql), nil
}

// ExecQuery runs a write query during database query execution.
func ExecQuery(sql string) error {
	if sql == "" {
		return ErrEmptyQuery
	}
	return nil
}

// prepareStmt compiles a query into a prepared statement before execution.
func prepareStmt(sql string) string {
	return "stmt:" + sql
}

// ErrEmptyQuery is returned when a database query is empty.
var ErrEmptyQuery = errQuery("empty query")

type errQuery string

func (e errQuery) Error() string { return string(e) }
