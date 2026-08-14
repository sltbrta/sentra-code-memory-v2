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
			// Extended exact-identifier probes (issue #54): broaden the
			// checked-in corpus so hit@k is measured across every fixture
			// package rather than a seven-file slice.
			{Name: "exact-new-router", Q: "NewRouter", Expect: []string{"api/router.go"}},
			{Name: "exact-logging-middleware", Q: "LoggingMiddleware", Expect: []string{"api/middleware.go"}},
			{Name: "exact-ok-response", Q: "OKResponse", Expect: []string{"api/response.go"}},
			{Name: "exact-begin-tx", Q: "BeginTx", Expect: []string{"db/tx.go"}},
			{Name: "exact-migrate", Q: "Migrate", Expect: []string{"db/migrate.go"}},
			{Name: "exact-authorize", Q: "Authorize", Expect: []string{"auth/rbac.go"}},
			{Name: "exact-new-user", Q: "NewUser", Expect: []string{"models/user.go"}},
			{Name: "exact-total-cents", Q: "TotalCents", Expect: []string{"models/order.go"}},
			{Name: "exact-new-counter", Q: "NewCounter", Expect: []string{"util/metrics.go"}},
			{Name: "exact-new-app-error", Q: "NewAppError", Expect: []string{"util/errors.go"}},
			{Name: "exact-end-session", Q: "EndSession", Expect: []string{"auth/session.go"}},
			{Name: "exact-default-retry-policy", Q: "DefaultRetryPolicy", Expect: []string{"util/retry.go"}},
			// Lexical probes: multi-word queries over content, with a distractor
			// (util/config.go mentions token/query) to exercise ranking.
			{Name: "lexical-token-validation", Q: "token validation claims", Expect: []string{"auth/token.go"}},
			{Name: "lexical-query-execution", Q: "database query execution prepared statement", Expect: []string{"db/query.go"}},
			{Name: "lexical-session-lifecycle", Q: "session lifecycle authenticated subject", Expect: []string{"auth/session.go", "auth/token.go"}},
			{Name: "lexical-transaction-commit", Q: "transaction stage commit rollback", Expect: []string{"db/tx.go"}},
			{Name: "lexical-role-action-permit", Q: "role action authorization permit", Expect: []string{"auth/rbac.go"}},
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
