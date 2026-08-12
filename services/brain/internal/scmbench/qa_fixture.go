package scmbench

// QAFixtureSuite returns the canonical retrieval-quality probe set for the
// checked-in qafixture corpus (issue #48). root is the fixture source tree and
// cache is a scratch index cache. The probe mix pairs exact-identifier queries
// (expected to hit at rank 1) with lexical queries that exercise ranking
// against distractor files, so hit@1/5/10 and precision are meaningful.
//
// Expect paths are repo-relative to root, matching codecrawl's slash paths.
func QAFixtureSuite(root, cache string) QASuite {
	return QASuite{
		Name:       "qafixture-retrieval",
		Root:       root,
		IndexCache: cache,
		Lane:       LaneLocalHeuristic,
		Queries: []QAQuery{
			// Exact-identifier probes: the named symbol lives in one file.
			{Name: "exact-validate-token", Q: "ValidateToken", Expect: []string{"auth/token.go"}},
			{Name: "exact-refresh-token", Q: "RefreshToken", Expect: []string{"auth/token.go"}},
			{Name: "exact-create-session", Q: "CreateSession", Expect: []string{"auth/session.go"}},
			{Name: "exact-run-query", Q: "RunQuery", Expect: []string{"db/query.go"}},
			{Name: "exact-open-conn", Q: "OpenConn", Expect: []string{"db/conn.go"}},
			{Name: "exact-handle-auth", Q: "HandleAuth", Expect: []string{"api/handler.go"}},
			{Name: "exact-log-error", Q: "LogError", Expect: []string{"util/log.go"}},
			// Lexical probes: multi-word queries over content, with a distractor
			// (util/config.go mentions token/query) to exercise ranking.
			{Name: "lexical-token-validation", Q: "token validation claims", Expect: []string{"auth/token.go"}},
			{Name: "lexical-query-execution", Q: "database query execution prepared statement", Expect: []string{"db/query.go"}},
			{Name: "lexical-session-lifecycle", Q: "session lifecycle authenticated subject", Expect: []string{"auth/session.go", "auth/token.go"}},
		},
	}
}

// QAFixtureThresholds are the baseline regression gates for the local
// heuristic lane on qafixture. They encode the current measured capability as
// a floor: exact identifiers must land at rank 1, the suite must stay fully
// retrievable at top-10, and the bounded context must beat reading the tree.
func QAFixtureThresholds() Thresholds {
	return Thresholds{
		MinHitRateAt1:        0.70,
		MinHitRateAt5:        0.90,
		MinHitRateAt10:       1.00,
		MaxP95LatencyMS:      2000,
		MinTokenSavingsRatio: 0.40,
		MaxFailedQueries:     0,
	}
}
