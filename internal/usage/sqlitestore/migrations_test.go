package sqlitestore

import (
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDiscoverMigrationsRequiresContiguousPrefixesFromOne(t *testing.T) {
	file := &fstest.MapFile{Data: []byte("SELECT 1;")}
	tests := []struct {
		name    string
		files   []string
		want    []string
		wantErr string
	}{
		{
			name:  "contiguous and sorted",
			files: []string{"migrations/002_b.sql", "migrations/001_a.sql", "migrations/003_c.sql", "migrations/README.md"},
			want:  []string{"migrations/001_a.sql", "migrations/002_b.sql", "migrations/003_c.sql"},
		},
		{name: "empty", files: []string{"migrations/README.md"}, wantErr: "no embedded usage migrations"},
		{name: "missing first", files: []string{"migrations/002_b.sql"}, wantErr: "want prefix 001_"},
		{name: "gap", files: []string{"migrations/001_a.sql", "migrations/003_c.sql"}, wantErr: "want prefix 002_"},
		{name: "duplicate prefix", files: []string{"migrations/001_a.sql", "migrations/001_b.sql"}, wantErr: "want prefix 002_"},
		{name: "unpadded prefix", files: []string{"migrations/1_a.sql"}, wantErr: "want prefix 001_"},
		{name: "missing separator", files: []string{"migrations/001a.sql"}, wantErr: "want prefix 001_"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := fstest.MapFS{}
			for _, name := range tc.files {
				files[name] = file
			}
			got, err := discoverMigrations(files)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("discoverMigrations = %v, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("discoverMigrations = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestSchemaVersionCountsEveryMigrationFile(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, "migrations/"+entry.Name())
		}
	}
	if SchemaVersion() != len(files) || !slices.Equal(migrationNames, files) {
		t.Fatalf("SchemaVersion() = %d with migrations %v, want every file %v", SchemaVersion(), migrationNames, files)
	}
}
