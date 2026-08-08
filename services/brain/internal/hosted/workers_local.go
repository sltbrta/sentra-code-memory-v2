package hosted

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// defaultLocalWorkers returns the local residual worker count for burst ingest,
// dense seed, and gardener drain when the operator did not pass --workers.
//
// This is the local analogue of hosted burst compute: fan out on the laptop
// (or local Postgres) instead of a remote fleet.
//
//	OUROBOROS_BRAIN_WORKERS=N  explicit override
//	else GOMAXPROCS (capped 1..32)
func defaultLocalWorkers() int {
	if v := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_WORKERS")); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			if n > 64 {
				return 64
			}
			return n
		}
	}
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	if n > 32 {
		n = 32
	}
	return n
}
