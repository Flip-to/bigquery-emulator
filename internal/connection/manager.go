package connection

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/goccy/googlesqlite"
)

type Manager struct {
	db    *sql.DB
	hooks *TxHooks
}

// TxHooks are called once a Tx ends. OnCommit runs after a successful
// commit, OnRollback after a rollback or a failed commit. State that must
// only become visible once the transaction is durable (for example a cache
// of rows written in it) is published or discarded from here.
type TxHooks struct {
	OnCommit   func(*sql.Tx)
	OnRollback func(*sql.Tx)
}

// SetTxHooks installs the hooks for every Tx begun afterwards.
func (m *Manager) SetTxHooks(hooks TxHooks) {
	m.hooks = &hooks
}

func NewManager(db *sql.DB) *Manager {
	return &Manager{db: db}
}

func (m *Manager) Connection(ctx context.Context, projectID, datasetID string) (*Conn, error) {
	if projectID == "" {
		return nil, fmt.Errorf("invalid projectID. projectID is empty")
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection: %w", err)
	}
	return &Conn{
		ProjectID: projectID,
		DatasetID: datasetID,
		Conn:      conn,
		hooks:     m.hooks,
	}, nil
}

type Tx struct {
	tx        *sql.Tx
	conn      *Conn
	committed bool
	hooksDone bool
}

func (t *Tx) Tx() *sql.Tx {
	return t.tx
}

func (t *Tx) RollbackIfNotCommitted() error {
	if t.committed {
		return nil
	}
	defer t.conn.Conn.Close()
	defer t.ended(false)
	return t.tx.Rollback()
}

func (t *Tx) Commit() error {
	if err := t.tx.Commit(); err != nil {
		t.ended(false)
		return err
	}
	t.committed = true
	t.ended(true)
	t.conn.Conn.Close()
	return nil
}

// ended runs the matching hook once per Tx.
func (t *Tx) ended(committed bool) {
	if t.hooksDone || t.conn.hooks == nil {
		return
	}
	t.hooksDone = true
	if committed {
		if t.conn.hooks.OnCommit != nil {
			t.conn.hooks.OnCommit(t.tx)
		}
		return
	}
	if t.conn.hooks.OnRollback != nil {
		t.conn.hooks.OnRollback(t.tx)
	}
}

func (t *Tx) SetProjectAndDataset(projectID, datasetID string) {
	t.conn.ProjectID = projectID
	t.conn.DatasetID = datasetID
}

func (t *Tx) MetadataRepoMode() error {
	if err := t.conn.Conn.Raw(func(c interface{}) error {
		gsqlConn, ok := c.(*googlesqlite.Conn)
		if !ok {
			return fmt.Errorf("failed to get *googlesqlite.Conn from %T", c)
		}
		_ = gsqlConn.SetNamePath([]string{})
		return nil
	}); err != nil {
		return fmt.Errorf("failed to setup connection: %w", err)
	}
	return nil
}

func (t *Tx) ContentRepoMode() error {
	if err := t.conn.Conn.Raw(func(c interface{}) error {
		gsqlConn, ok := c.(*googlesqlite.Conn)
		if !ok {
			return fmt.Errorf("failed to get *googlesqlite.Conn from %T", c)
		}
		if t.conn.DatasetID == "" {
			_ = gsqlConn.SetNamePath([]string{t.conn.ProjectID})
		} else {
			_ = gsqlConn.SetNamePath([]string{t.conn.ProjectID, t.conn.DatasetID})
		}
		const maxNamePath = 3 // projectID and datasetID and tableID
		gsqlConn.SetMaxNamePath(maxNamePath)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to setup connection: %w", err)
	}
	return nil
}

type Conn struct {
	ProjectID string
	DatasetID string
	Conn      *sql.Conn
	hooks     *TxHooks
}

func (c *Conn) Begin(ctx context.Context) (*Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, nil)
	if err != nil {
		// The pooled connection is owned by the Tx once BeginTx succeeds and
		// is released by Commit/RollbackIfNotCommitted. When BeginTx fails no
		// Tx is created, so the connection must be returned to the pool here
		// or it leaks for the lifetime of the process.
		_ = c.Conn.Close()
		return nil, err
	}
	return &Tx{tx: tx, conn: c}, nil
}
