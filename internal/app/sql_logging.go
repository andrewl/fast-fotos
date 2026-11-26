package app

import (
	"context"
	"database/sql"
	"log/slog"
	"runtime"
	"time"
)

type sqlQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type loggedQuerier struct {
	querier sqlQuerier
}

// logSQL emits the SQL statement with the calling database function's source location.
// Inputs: context and SQL text; output: none.
func logSQL(ctx context.Context, query string) {
	logger := slog.Default()
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	record := slog.NewRecord(time.Now(), slog.LevelDebug, "execute SQL", pcs[0])
	record.AddAttrs(slog.String("sql", query))
	_ = logger.Handler().Handle(ctx, record)
}

// ExecContext logs and executes a statement with its arguments.
// Inputs: context, SQL text, and arguments; outputs: the result or error.
func (q loggedQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	logSQL(ctx, query)
	return q.querier.ExecContext(ctx, query, args...)
}

// QueryContext logs and executes a query with its arguments.
// Inputs: context, SQL text, and arguments; outputs: rows or an error.
func (q loggedQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	logSQL(ctx, query)
	return q.querier.QueryContext(ctx, query, args...)
}

// QueryRowContext logs and executes a single-row query with its arguments.
// Inputs: context, SQL text, and arguments; output: the resulting row.
func (q loggedQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	logSQL(ctx, query)
	return q.querier.QueryRowContext(ctx, query, args...)
}

type loggedDB struct {
	loggedQuerier
	db *sql.DB
}

// BeginTx starts a transaction whose statements are logged.
// Inputs: context and transaction options; outputs: the transaction or an error.
func (d *loggedDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*loggedTx, error) {
	tx, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &loggedTx{loggedQuerier: loggedQuerier{querier: tx}, tx: tx}, nil
}

// Close closes the underlying database.
// Input: the database; output: a close error, if any.
func (d *loggedDB) Close() error {
	return d.db.Close()
}

type loggedTx struct {
	loggedQuerier
	tx *sql.Tx
}

// PrepareContext prepares a statement whose executions are logged.
// Inputs: context and SQL text; outputs: the prepared statement or an error.
func (t *loggedTx) PrepareContext(ctx context.Context, query string) (*loggedStmt, error) {
	stmt, err := t.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &loggedStmt{stmt: stmt, query: query}, nil
}

// Commit commits the transaction.
// Input: the transaction; output: a commit error, if any.
func (t *loggedTx) Commit() error {
	return t.tx.Commit()
}

// Rollback rolls back the transaction.
// Input: the transaction; output: a rollback error, if any.
func (t *loggedTx) Rollback() error {
	return t.tx.Rollback()
}

type loggedStmt struct {
	stmt  *sql.Stmt
	query string
}

// ExecContext logs and executes the prepared statement.
// Inputs: context and arguments; outputs: the result or an error.
func (s *loggedStmt) ExecContext(ctx context.Context, args ...any) (sql.Result, error) {
	logSQL(ctx, s.query)
	return s.stmt.ExecContext(ctx, args...)
}

// Close releases the prepared statement.
// Input: the statement; output: a close error, if any.
func (s *loggedStmt) Close() error {
	return s.stmt.Close()
}
