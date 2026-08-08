package companymode

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// LoadResult is the 20-user mixed-load smoke receipt.
type LoadResult struct {
	Users          int            `json:"users"`
	QueryAttempts  int64          `json:"query_attempts"`
	IngestAttempts int64          `json:"ingest_attempts"`
	QueryOK        int64          `json:"query_ok"`
	IngestOK       int64          `json:"ingest_ok"`
	Denied         int64          `json:"denied"`
	QueueRejected  int64          `json:"queue_rejected"`
	ACLBypass      int64          `json:"acl_bypass"`
	Status         string         `json:"status"`
	Mix            map[string]int `json:"mix"`
}

// RunMixedLoadSmoke simulates 20 mixed query/ingest users without ACL bypass.
// Cross-tenant and unauthorized principals are denied and counted.
func RunMixedLoadSmoke(ctx context.Context, runtime *Runtime) (LoadResult, error) {
	alice, bob := runtime.Principals()
	result := LoadResult{
		Users: 20,
		Mix: map[string]int{
			"query":  12,
			"ingest": 6,
			"deny":   2,
		},
	}

	var (
		queryAttempts, ingestAttempts atomic.Int64
		queryOK, ingestOK             atomic.Int64
		denied, queueRejected, bypass atomic.Int64
	)

	var wg sync.WaitGroup
	// 12 query workers alternating alice/bob.
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			principal := alice
			if i%2 == 1 {
				principal = bob
			}
			queryAttempts.Add(1)
			_, err := runtime.Query(ctx, principal)
			if err == nil {
				queryOK.Add(1)
				return
			}
			if err == ErrQueueFull {
				queueRejected.Add(1)
				return
			}
			denied.Add(1)
		}(i)
	}
	// 6 ingest workers (alice owner only succeeds; bob viewer denies).
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			principal := alice
			if i%3 == 0 {
				principal = bob // viewer cannot ingest
			}
			ingestAttempts.Add(1)
			err := runtime.Ingest(ctx, principal, fmt.Sprintf("evt-%d", i), fmt.Sprintf("blob-%d", i), []byte(fmt.Sprintf("payload-%d", i)))
			if err == nil {
				ingestOK.Add(1)
				return
			}
			if err == ErrQueueFull {
				queueRejected.Add(1)
				return
			}
			denied.Add(1)
		}(i)
	}
	// 2 explicit cross-tenant / stranger denials — ACL must not bypass.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stranger := Principal{ID: "eve", TenantID: "other-co"}
			_, err := runtime.Query(ctx, stranger)
			if err == nil {
				bypass.Add(1)
				return
			}
			denied.Add(1)
			// Also try to read company blob as wrong tenant.
			_, err = runtime.ReadBlob(ctx, stranger, "blob-0")
			if err == nil {
				bypass.Add(1)
			}
		}(i)
	}
	wg.Wait()

	result.QueryAttempts = queryAttempts.Load()
	result.IngestAttempts = ingestAttempts.Load()
	result.QueryOK = queryOK.Load()
	result.IngestOK = ingestOK.Load()
	result.Denied = denied.Load()
	result.QueueRejected = queueRejected.Load()
	result.ACLBypass = bypass.Load()
	if result.ACLBypass > 0 {
		result.Status = "failed_acl_bypass"
		return result, ErrDenied
	}
	if result.QueryOK == 0 || result.IngestOK == 0 {
		result.Status = "failed_throughput"
		return result, ErrRejected
	}
	result.Status = "passed"
	return result, nil
}
