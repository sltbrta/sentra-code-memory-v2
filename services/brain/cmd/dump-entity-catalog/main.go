// dump-entity-catalog builds offline entity-catalog.gob from Neon path2_entities
// for HotLex volume mount (Modal: /hotlex/entity-catalog.gob).
//
//	NEON_DATABASE_URL=… OUROBOROS_ERB_BRAIN_ID=full-bench-v2 \
//	  go run ./services/brain/cmd/dump-entity-catalog -o /hotlex/entity-catalog.gob
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func main() {
	out := flag.String("o", "entity-catalog.gob", "output path (.gob or .json)")
	limit := flag.Int("limit", 80000, "max entity rows")
	generation := flag.String("generation", strings.TrimSpace(os.Getenv("OUROBOROS_ERB_GENERATION_ID")),
		"optional serving generation id used to reject stale catalogs")
	flag.Parse()
	brain := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_BRAIN_ID"))
	if brain == "" {
		brain = "full-bench-v2"
	}
	dsn := strings.TrimSpace(os.Getenv("NEON_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "NEON_DATABASE_URL or DATABASE_URL required")
		os.Exit(2)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(2)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ping: %v\n", err)
		os.Exit(2)
	}
	cat, err := hosted.DumpEntityCatalogFromDB(ctx, db, brain, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dump: %v\n", err)
		os.Exit(2)
	}
	cat.Generation = strings.TrimSpace(*generation)
	if err := hosted.WriteOfflineEntityCatalog(*out, cat); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "wrote %s names=%d dsid_keys=%d brain=%s generation=%s\n",
		*out, len(cat.Names), len(cat.NameToDSIDs), brain, cat.Generation)
}
