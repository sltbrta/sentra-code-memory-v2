package db

// OpenConn opens a database connection to the given data source name.
// Connection pooling is managed by the caller.
func OpenConn(dsn string) (*Conn, error) {
	if dsn == "" {
		return nil, ErrEmptyDSN
	}
	return &Conn{dsn: dsn}, nil
}

// CloseConn closes a database connection and releases its resources.
func CloseConn(c *Conn) error {
	if c == nil {
		return nil
	}
	c.closed = true
	return nil
}

// Conn is a single database connection.
type Conn struct {
	dsn    string
	closed bool
}

// ErrEmptyDSN is returned when a connection data source is empty.
var ErrEmptyDSN = errConn("empty dsn")

type errConn string

func (e errConn) Error() string { return string(e) }
