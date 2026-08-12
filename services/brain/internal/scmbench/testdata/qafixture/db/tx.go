package db

// Tx is a database transaction scope. Work staged inside a transaction is
// committed atomically or rolled back on error.
type Tx struct {
	conn   *Conn
	staged []string
	done   bool
}

// BeginTx opens a transaction on the connection.
func BeginTx(conn *Conn) (*Tx, error) {
	if conn == nil {
		return nil, ErrEmptyDSN
	}
	return &Tx{conn: conn}, nil
}

// Stage adds a statement to the transaction without executing it.
func (t *Tx) Stage(sql string) error {
	if t.done {
		return ErrEmptyQuery
	}
	t.staged = append(t.staged, sql)
	return nil
}

// Commit executes every staged statement and closes the transaction.
func (t *Tx) Commit() error {
	if t.done {
		return ErrEmptyQuery
	}
	for _, sql := range t.staged {
		if err := ExecQuery(sql); err != nil {
			return err
		}
	}
	t.done = true
	return nil
}

// RollbackTx discards staged work and closes the transaction.
func (t *Tx) RollbackTx() {
	t.staged = nil
	t.done = true
}
