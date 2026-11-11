//go:build integration

package migrate_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cferrante/call-event-pipeline/internal/dbtest"
	"github.com/cferrante/call-event-pipeline/internal/migrate"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func file(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }

func count(t *testing.T, db *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(ctx(t), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// Files are applied in name order, regardless of the order the filesystem lists them.
func TestUpAppliesFilesInNameOrder(t *testing.T) {
	db := dbtest.Empty(t)
	fsys := fstest.MapFS{
		"0002_add_row.sql":     file("INSERT INTO t VALUES (2)"),
		"0001_create_t.sql":    file("CREATE TABLE t (v int); INSERT INTO t VALUES (1)"),
		"README.md":            file("not a migration"),
		"0003_another_row.sql": file("INSERT INTO t VALUES (3)"),
	}

	applied, err := migrate.Up(ctx(t), db, fsys)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	want := []string{"0001_create_t.sql", "0002_add_row.sql", "0003_another_row.sql"}
	if !reflect.DeepEqual(applied, want) {
		t.Errorf("applied = %v, want %v", applied, want)
	}
	if n := count(t, db, "SELECT count(*) FROM t"); n != 3 {
		t.Errorf("rows in t = %d, want 3", n)
	}
}

func TestUpSkipsAlreadyAppliedFiles(t *testing.T) {
	db := dbtest.Empty(t)
	first := fstest.MapFS{"0001_create_t.sql": file("CREATE TABLE t (v int)")}
	if _, err := migrate.Up(ctx(t), db, first); err != nil {
		t.Fatalf("first Up: %v", err)
	}

	again, err := migrate.Up(ctx(t), db, first)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Up applied %v, want nothing", again)
	}

	second := fstest.MapFS{
		"0001_create_t.sql": file("CREATE TABLE t (v int)"),
		"0002_add_row.sql":  file("INSERT INTO t VALUES (1)"),
	}
	applied, err := migrate.Up(ctx(t), db, second)
	if err != nil {
		t.Fatalf("Up with new file: %v", err)
	}
	if !reflect.DeepEqual(applied, []string{"0002_add_row.sql"}) {
		t.Errorf("applied = %v, want only the new file", applied)
	}
}

// A failing migration must leave no trace: none of its statements, and no record that it ran.
func TestUpRollsBackAFailedMigration(t *testing.T) {
	db := dbtest.Empty(t)
	fsys := fstest.MapFS{
		"0001_create_t.sql": file("CREATE TABLE t (v int)"),
		"0002_broken.sql":   file("CREATE TABLE u (v int); SELECT no_such_column FROM t"),
	}

	_, err := migrate.Up(ctx(t), db, fsys)
	if err == nil {
		t.Fatal("Up succeeded, want error from 0002_broken.sql")
	}
	if !strings.Contains(err.Error(), "0002_broken.sql") {
		t.Errorf("error %q does not name the failing file", err)
	}
	if n := count(t, db, "SELECT count(*) FROM pg_tables WHERE tablename = 'u'"); n != 0 {
		t.Error("table u exists: the failed migration was partly applied")
	}
	if n := count(t, db, "SELECT count(*) FROM schema_migrations WHERE version = '0002_broken.sql'"); n != 0 {
		t.Error("failed migration was recorded as applied")
	}
	if n := count(t, db, "SELECT count(*) FROM schema_migrations WHERE version = '0001_create_t.sql'"); n != 1 {
		t.Error("earlier successful migration was not recorded")
	}
}

// A file's changes and its schema_migrations row must commit together. Postgres already
// makes one multi-statement file atomic, so this forces the *recording* step to fail: the
// migration installs a trigger that rejects its own row.
func TestUpCommitsFileAndRecordTogether(t *testing.T) {
	db := dbtest.Empty(t)
	fsys := fstest.MapFS{"0001_unrecordable.sql": file(`
		CREATE TABLE u (v int);
		CREATE FUNCTION reject() RETURNS trigger LANGUAGE plpgsql AS
			$$ BEGIN RAISE EXCEPTION 'recording rejected'; END $$;
		CREATE TRIGGER reject BEFORE INSERT ON schema_migrations
			FOR EACH ROW EXECUTE FUNCTION reject();
	`)}

	if _, err := migrate.Up(ctx(t), db, fsys); err == nil {
		t.Fatal("Up succeeded, want error from the rejected record")
	}
	if n := count(t, db, "SELECT count(*) FROM pg_tables WHERE tablename = 'u'"); n != 0 {
		t.Error("table u exists: the migration applied but was never recorded, so it would run again")
	}
}

// Both services run migrations at startup and may start at the same moment.
func TestUpRunsSafelyInParallel(t *testing.T) {
	db := dbtest.Empty(t)
	fsys := fstest.MapFS{"0001_create_t.sql": file("CREATE TABLE t (v int)")}

	var wg sync.WaitGroup
	results := make([][]string, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Go(func() { results[i], errs[i] = migrate.Up(ctx(t), db, fsys) })
	}
	wg.Wait()

	total := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("Up %d: %v", i, err)
		}
		total += len(results[i])
	}
	if total != 1 {
		t.Errorf("migration applied %d times in total, want 1", total)
	}
}
