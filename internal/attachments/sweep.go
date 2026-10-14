package attachments

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fileDeleter is UploadThing's delete API (a fake in tests).
type fileDeleter interface {
	DeleteFiles(ctx context.Context, keys ...string) error
}

// Sweeper deletes files from UploadThing once their attachment is gone, however it went: removed by
// hand, or with its ticket, project or organization. A database trigger queues each file key in
// deleted_files, so no delete path can forget its files (TL-206).
type Sweeper struct {
	DB    *pgxpool.Pool
	Files fileDeleter
}

// KeepDays is how long a deleted file waits in its day's batch before it's deleted from storage, so a
// mistake can still be undone (TL-256): the batch dated D goes on day D+30.
const KeepDays = 30

// Sweep deletes up to 100 queued files whose day's batch is due; a failed batch stays queued and is
// retried next time.
func (s Sweeper) Sweep(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT file_key FROM deleted_files WHERE deleted_on <= current_date - $1::int
		ORDER BY deleted_on, deleted_at LIMIT 100`, KeepDays)
	if err != nil {
		return 0, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(keys) == 0 {
		return 0, err
	}
	if err := s.Files.DeleteFiles(ctx, keys...); err != nil {
		_, _ = s.DB.Exec(ctx, `UPDATE deleted_files SET attempts = attempts + 1 WHERE file_key = ANY($1)`, keys)
		return 0, err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM deleted_files WHERE file_key = ANY($1)`, keys)
	return len(keys), err
}

// SweepAll deletes every due file, a batch of 100 at a time, until none are left or one fails.
func (s Sweeper) SweepAll(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.Sweep(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}
