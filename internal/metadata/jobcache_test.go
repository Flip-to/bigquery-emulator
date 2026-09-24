package metadata_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	bigqueryv2 "google.golang.org/api/bigquery/v2"

	"github.com/goccy/bigquery-emulator/internal/connection"
	"github.com/goccy/bigquery-emulator/internal/metadata"
	_ "github.com/goccy/googlesqlite"
)

// A job added inside a transaction that rolls back must not stay in the
// recent-jobs cache: the client retries with the same job ID, and that retry
// was rejected with "job ... is already created" although the job never
// reached the jobs table.
func TestJobCacheRollback(t *testing.T) {
	ctx := context.Background()
	// Not t.TempDir: on Windows the driver may still hold the file when the
	// test's cleanup runs, which fails the test.
	f, err := os.CreateTemp("", "jobcache-*.db")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	defer os.Remove(f.Name())
	db, err := sql.Open("googlesqlite", "file:"+f.Name()+"?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := metadata.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	mgr := connection.NewManager(db)
	mgr.SetTxHooks(connection.TxHooks{OnCommit: repo.TxCommitted, OnRollback: repo.TxRolledBack})

	const projectID = "test"
	newJob := func(id string) *metadata.Job {
		return metadata.NewJob(repo, projectID, id, &bigqueryv2.Job{
			JobReference: &bigqueryv2.JobReference{ProjectId: projectID, JobId: id},
		}, nil, nil)
	}
	// addJob adds the job in a fresh request-scoped Project, as the server
	// middleware does per request, then commits or rolls back.
	addJob := func(id string, commit bool) error {
		conn, err := mgr.Connection(ctx, projectID, "")
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.RollbackIfNotCommitted()
		if err := metadata.NewProject(repo, projectID, nil, nil).AddJob(ctx, tx.Tx(), newJob(id)); err != nil {
			return err
		}
		if commit {
			return tx.Commit()
		}
		return nil
	}

	if err := addJob("job1", false); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if job, err := repo.FindJob(ctx, projectID, "job1"); err != nil || job != nil {
		t.Fatalf("rolled-back job visible: job=%v err=%v", job, err)
	}
	if err := addJob("job1", true); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if job, err := repo.FindJob(ctx, projectID, "job1"); err != nil || job == nil {
		t.Fatalf("committed job not found: job=%v err=%v", job, err)
	}
	if err := addJob("job1", true); err == nil {
		t.Fatal("adding a committed job ID again succeeded, want already created")
	}
}
