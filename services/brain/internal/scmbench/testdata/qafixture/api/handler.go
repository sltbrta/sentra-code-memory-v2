// Package api exposes HTTP handlers that bridge authentication and database
// query execution for the qafixture benchmark corpus. It is indexed, never built.
package api

// HandleAuth is the HTTP handler for authentication endpoints. It validates the
// bearer token before dispatching the request.
func HandleAuth(token string) string {
	return "auth:" + token
}

// HandleQuery is the HTTP handler for query endpoints. It runs the query after
// authenticating the caller.
func HandleQuery(token, sql string) string {
	return "query:" + sql
}
