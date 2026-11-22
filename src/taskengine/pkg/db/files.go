package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"taskengine/pkg/models"
)

// CreateFile inserts a stored-object record.
func (d *DB) CreateFile(ctx context.Context, f *models.File) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	meta := []byte("{}")
	if len(f.Meta) > 0 {
		meta, _ = json.Marshal(f.Meta)
	}
	var taskID interface{}
	if f.TaskID != "" {
		taskID = f.TaskID
	}
	_, err := d.db.ExecContext(ctx, `
	INSERT INTO files (id, plugin, task_id, kind, object_key, filename, mime, size, status, meta, created_at, expires_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.Plugin, taskID, f.Kind, f.ObjectKey, f.Filename, f.MIME, f.Size, f.Status, string(meta), f.CreatedAt.UTC(), f.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert file: %w", err)
	}
	return nil
}

const fileColumns = `id, plugin, task_id, kind, object_key, filename, mime, size, status, meta, created_at, expires_at`

func scanFile(sc interface{ Scan(...any) error }) (*models.File, error) {
	var f models.File
	var taskID sql.NullString
	var meta string
	if err := sc.Scan(&f.ID, &f.Plugin, &taskID, &f.Kind, &f.ObjectKey, &f.Filename, &f.MIME, &f.Size, &f.Status, &meta, &f.CreatedAt, &f.ExpiresAt); err != nil {
		return nil, err
	}
	f.TaskID = taskID.String
	if meta != "" && meta != "{}" {
		_ = json.Unmarshal([]byte(meta), &f.Meta)
	}
	return &f, nil
}

// GetFile returns a file record or nil if missing.
func (d *DB) GetFile(ctx context.Context, id string) (*models.File, error) {
	f, err := scanFile(d.db.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM files WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get file %s: %w", id, err)
	}
	return f, nil
}

// ListTaskFiles returns output files attached to a task, oldest first.
func (d *DB) ListTaskFiles(ctx context.Context, taskID string) ([]models.File, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+fileColumns+` FROM files WHERE task_id = ? ORDER BY created_at ASC, filename ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task files: %w", err)
	}
	defer rows.Close()
	files := []models.File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	return files, rows.Err()
}

// MarkFileReady records a confirmed upload with its actual size.
func (d *DB) MarkFileReady(ctx context.Context, id string, size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, `UPDATE files SET status = 'READY', size = ? WHERE id = ? AND status != 'EXPIRED'`, size, id)
	return err
}

// ExpireFiles marks files past their retention as EXPIRED, plus PENDING
// files whose upload was never confirmed within pendingGrace.
func (d *DB) ExpireFiles(ctx context.Context, now time.Time, pendingGrace time.Duration) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.ExecContext(ctx, `
	UPDATE files SET status = 'EXPIRED'
	WHERE status != 'EXPIRED' AND (expires_at <= ? OR (status = 'PENDING' AND created_at <= ?))`,
		now.UTC(), now.Add(-pendingGrace).UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
