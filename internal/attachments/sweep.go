package attachments

import (
	"context"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
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

// Sweep deletes up to 100 queued files; a failed batch stays queued and is retried next time.
func (s Sweeper) Sweep(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT file_key FROM deleted_files ORDER BY deleted_at LIMIT 100`)
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

// Run sweeps every interval until ctx ends.
func (s Sweeper) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			logs.Errorf("attachments: delete files from UploadThing: %v", err)
		} else if n > 0 {
			logs.Logf("attachments: deleted %d file(s) from UploadThing", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
