package store

import (
	"fmt"
	"io/fs"
	"sort"
	"testing"
)

// TestMigrationNumbersUniqueAndContiguous guards the one migration mistake
// that does not fail loudly: migrate() records a migration under the integer
// parsed from its filename, so a second file carrying a number already in
// schema_migrations is skipped in silence rather than rejected. Two clusters
// numbering their migration 0010 would ship a database missing half its DDL.
func TestMigrationNumbersUniqueAndContiguous(t *testing.T) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	seen := map[int]string{}
	versions := make([]int, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			t.Fatalf("migration %q does not start with a number: %v", name, err)
		}
		if version < 1 {
			t.Errorf("migration %q has version %d; versions start at 1", name, version)
		}
		if other, dup := seen[version]; dup {
			t.Errorf("migrations %q and %q share version %d; the second would be applied silently never", other, name, version)
		}
		seen[version] = name
		versions = append(versions, version)
	}
	if len(versions) == 0 {
		t.Fatal("no migrations found")
	}
	sort.Ints(versions)
	for i, v := range versions {
		if want := i + 1; v != want {
			t.Fatalf("migration versions are not contiguous: expected %d, found %d (%s)", want, v, seen[v])
		}
	}
}
