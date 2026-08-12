// Command qafixture wires the auth, db, and api packages into one entry point
// for the benchmark corpus. It is indexed, never built.
package main

import (
	"qafixture/api"
	"qafixture/auth"
	"qafixture/db"
	"qafixture/util"
)

func main() {
	cfg := util.LoadConfig()
	util.LogInfo("starting " + cfg.TokenName)

	subject, err := auth.ValidateToken("demo-token")
	if err != nil {
		util.LogError("token validation failed", err)
		return
	}
	session := auth.CreateSession(subject)

	conn, err := db.OpenConn("memory://")
	if err != nil {
		util.LogError("connection failed", err)
		return
	}
	defer func() { _ = db.CloseConn(conn) }()

	if _, err := db.RunQuery("SELECT 1"); err != nil {
		util.LogError("query execution failed", err)
		return
	}

	_ = api.HandleAuth(session)
	_ = api.HandleQuery(session, "SELECT")
	util.LogInfo("done")
}
