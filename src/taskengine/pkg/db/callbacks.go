package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"taskengine/pkg/models"

	"github.com/google/uuid"
)

// enqueueCallback queues a webhook call for a task that just reached a final
// state. Tasks without a callback are skipped.
func enqueueCallback(ctx context.Context, tx *sql.Tx, taskID, event string) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
	INSERT INTO callbacks (id, task_id, event, url, ref, status, next_attempt_at, created_at)
	SELECT ?, id, ?, json_extract(callback, '$.url'), COALESCE(json_extract(callback, '$.ref'), ''), 'PENDING', ?, ?
	FROM tasks
	WHERE id = ? AND callback != '' AND COALESCE(json_extract(callback, '$.url'), '') != ''
	`, uuid.New().String(), event, now, now, taskID)
	if err != nil {
		return fmt.Errorf("queue callback for task %s: %w", taskID, err)
	}
	return nil
}

const callbackColumns = `id, task_id, event, url, ref, status, attempts, last_status, last_error, next_attempt_at, created_at, delivered_at`

func scanCallback(row interface{ Scan(...interface{}) error }) (models.CallbackDelivery, error) {
	var c models.CallbackDelivery
	var delivered sql.NullTime
	err := row.Scan(&c.ID, &c.TaskID, &c.Event, &c.URL, &c.Ref, &c.Status, &c.Attempts, &c.LastStatus, &c.LastError, &c.NextAttemptAt, &c.CreatedAt, &delivered)
	if delivered.Valid {
		c.DeliveredAt = &delivered.Time
	}
	return c, err
}

func (d *DB) queryCallbacks(ctx context.Context, query string, args ...interface{}) ([]models.CallbackDelivery, error) {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.CallbackDelivery
	for rows.Next() {
		c, err := scanCallback(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DueCallbacks returns pending callbacks whose next attempt is due, oldest first.
func (d *DB) DueCallbacks(ctx context.Context, now time.Time, limit int) ([]models.CallbackDelivery, error) {
	if limit <= 0 {
		limit = 20
	}
	return d.queryCallbacks(ctx, `SELECT `+callbackColumns+` FROM callbacks
	WHERE status = 'PENDING' AND next_attempt_at <= ?
	ORDER BY next_attempt_at ASC LIMIT ?`, now.UTC(), limit)
}

// ListTaskCallbacks returns a task's callback deliveries, oldest first.
func (d *DB) ListTaskCallbacks(ctx context.Context, taskID string) ([]models.CallbackDelivery, error) {
	return d.queryCallbacks(ctx, `SELECT `+callbackColumns+` FROM callbacks WHERE task_id = ? ORDER BY created_at ASC`, taskID)
}

// GetCallback returns one callback delivery, or nil if it doesn't exist.
func (d *DB) GetCallback(ctx context.Context, id string) (*models.CallbackDelivery, error) {
	c, err := scanCallback(d.db.QueryRowContext(ctx, `SELECT `+callbackColumns+` FROM callbacks WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MarkCallbackDelivered records a successful delivery.
func (d *DB) MarkCallbackDelivered(ctx context.Context, id string, httpStatus int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().UTC()
	_, err := d.db.ExecContext(ctx, `
	UPDATE callbacks SET status = 'DELIVERED', attempts = attempts + 1, last_status = ?, last_error = '', delivered_at = ?
	WHERE id = ? AND status = 'PENDING'`, httpStatus, now, id)
	return err
}

// MarkCallbackAttemptFailed records a failed attempt. A nil next schedules no
// retry and marks the callback FAILED.
func (d *DB) MarkCallbackAttemptFailed(ctx context.Context, id string, httpStatus int, errMsg string, next *time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	if next == nil {
		_, err := d.db.ExecContext(ctx, `
		UPDATE callbacks SET status = 'FAILED', attempts = attempts + 1, last_status = ?, last_error = ?
		WHERE id = ? AND status = 'PENDING'`, httpStatus, errMsg, id)
		return err
	}
	_, err := d.db.ExecContext(ctx, `
	UPDATE callbacks SET attempts = attempts + 1, last_status = ?, last_error = ?, next_attempt_at = ?
	WHERE id = ? AND status = 'PENDING'`, httpStatus, errMsg, next.UTC(), id)
	return err
}

// RedeliverCallback puts a callback back in the queue to be sent right away
// (e.g. after the receiver was fixed). Attempts restart from zero.
func (d *DB) RedeliverCallback(ctx context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.ExecContext(ctx, `
	UPDATE callbacks SET status = 'PENDING', attempts = 0, last_error = '', next_attempt_at = ?, delivered_at = NULL
	WHERE id = ?`, time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
