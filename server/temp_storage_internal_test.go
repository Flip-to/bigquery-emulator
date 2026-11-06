package server

import (
	"context"
	"strings"
	"testing"
)

// TestTempStorageSkipsFsync checks that the throwaway TempStorage database
// is opened with synchronous=off and an in-memory journal. Without them every
// commit fsynced a file that is deleted at shutdown, about 40% of a small
// query's time.
func TestTempStorageSkipsFsync(t *testing.T) {
	s, err := New(TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop(context.Background())

	dsn := string(s.storage)
	for _, want := range []string{"_pragma=synchronous(off)", "cache=shared"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("TempStorage DSN %q lacks %q", dsn, want)
		}
	}
	// TempStorage must keep WAL mode, which concurrent reads depend on: a
	// journal_mode in the DSN would make withConcurrencyPragmas skip it.
	if !s.walMode {
		t.Errorf("TempStorage is not in WAL mode (DSN %q), so requests cannot run concurrently", dsn)
	}
	// The database must still open and accept writes with these pragmas.
	if err := s.SetProject("p"); err != nil {
		t.Fatalf("SetProject on TempStorage: %v", err)
	}
}
