// Package schema embeds the immutable, ordered local-authority migrations.
// Callers receive fresh descriptors so runtime startup never depends on source
// paths and cannot mutate the package's canonical migration sequence.
package schema

import (
	_ "embed"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
)

var (
	//go:embed migrations/001_stage02_authority.sql
	migration001 string
	//go:embed migrations/002_durable_storage_adapters.sql
	migration002 string
	//go:embed migrations/003_stage03_ingestion.sql
	migration003 string
	//go:embed migrations/004_stage04_conversation.sql
	migration004 string
	//go:embed migrations/005_stage05_factory.sql
	migration005 string
	//go:embed migrations/006_stage07_meetings.sql
	migration006 string
	//go:embed migrations/007_stage11_multimodal.sql
	migration007 string
)

// Migrations returns the complete ordered local-state schema. The returned slice
// is independent on every call; applying it has SQLite side effects, while
// obtaining it performs no I/O and cannot fail.
func Migrations() []localstate.Migration {
	return []localstate.Migration{
		{Version: 1, SQL: migration001},
		{Version: 2, SQL: migration002},
		{Version: 3, SQL: migration003},
		{Version: 4, SQL: migration004},
		{Version: 5, SQL: migration005},
		{Version: 6, SQL: migration006},
		{Version: 7, SQL: migration007},
	}
}
