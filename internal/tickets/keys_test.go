package tickets

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/projects"
)

func TestTicketKeys(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id IN ('ky1', 'ky2'); DELETE FROM projects WHERE owner_clerk_id IN ('ky1', 'ky2')`); err != nil {
		t.Fatal(err)
	}
	project := func(org, name string) (id, prefix string) {
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, $2) RETURNING id, key_prefix`, org, name).Scan(&id, &prefix); err != nil {
			t.Fatal(err)
		}
		return
	}
	// prefixes from the name, unique per org
	pid, tl := project("ky1", "TrackLines")
	_, tl2 := project("ky1", "TrackLines Mobile")
	_, other := project("ky2", "TrackLines")
	_, flags := project("ky1", "flags.gg")
	if tl != "TL" || tl2 != "TLM" || other != "TL" || flags != "FLA" {
		t.Fatalf("prefixes: %s %s %s %s", tl, tl2, other, flags)
	}
	// a name starting with a digit still gets a letter-first prefix (TL-181: 2FA broke the insert)
	_, digit := project("ky1", "2fa")
	_, digits := project("ky1", "42")
	_, none := project("ky1", "!!")
	if digit != "P2FA" || digits != "P42" || none != "PX" {
		t.Fatalf("letter first: %s %s %s", digit, digits, none)
	}
	_, again := project("ky1", "Track Lines") // capitals TL taken in ky1
	if again != "TL2" {
		t.Fatalf("collision: %s", again)
	}

	// numbers per project, on every create path
	b, _ := boards.Store{DB: db}.Create(ctx, "ky1", pid, "b", "")
	s := Store{DB: db}
	first, _ := s.CreateBacklog(ctx, "ky1", pid, "task", "one", "")
	second, _ := s.Create(ctx, "ky1", b.Columns[0].ID, "task", "two", "")
	if first.Key != "TL-1" || second.Key != "TL-2" {
		t.Fatalf("keys: %s %s", first.Key, second.Key)
	}
	// concurrent creates still get distinct numbers
	var wg sync.WaitGroup
	keys := sync.Map{}
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, err := s.CreateBacklog(ctx, "ky1", pid, "task", fmt.Sprint(i), "")
			if err != nil {
				t.Error(err)
				return
			}
			if _, dup := keys.LoadOrStore(tk.Key, true); dup {
				t.Errorf("duplicate key %s", tk.Key)
			}
		}()
	}
	wg.Wait()
	if d, _ := s.Get(ctx, "ky1", second.ID); d.Key != "TL-2" {
		t.Fatalf("detail key: %q", d.Key)
	}
	open, _ := s.OpenTickets(ctx, "ky1", pid, OpenFilter{})
	if len(open) == 0 || !strings.HasPrefix(open[0].Key, "TL-") {
		t.Fatalf("open tickets carry keys: %+v", open)
	}

	// lookup: case-insensitive, org-scoped
	if id, err := s.IDByKey(ctx, "ky1", "tl-2"); err != nil || id != second.ID {
		t.Fatalf("by key: %s %v", id, err)
	}
	for _, bad := range []string{"TL-999", "nonsense", "TL2-1"} {
		if _, err := s.IDByKey(ctx, "ky1", bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := s.IDByKey(ctx, "ky2", "TL-2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("another org's ticket resolved")
	}
	h := NewSystem(db)
	req := httptest.NewRequest("GET", "/", nil).WithContext(auth.WithAPIKeyUser(ctx, "ky1", "claude"))
	req.SetPathValue("key", "TL-1")
	rec := httptest.NewRecorder()
	h.GetByKey(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), first.ID) {
		t.Fatalf("http by key: %d %s", rec.Code, rec.Body)
	}

	// renaming the prefix renames every key; bad or taken prefixes are refused
	ps := projects.Store{DB: db}
	if err := ps.SetKeyPrefix(ctx, pid, "ky1", "TRK"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "ky1", first.ID); d.Key != "TRK-1" {
		t.Fatalf("renamed: %s", d.Key)
	}
	if err := ps.SetKeyPrefix(ctx, pid, "ky1", "tl-x"); !errors.Is(err, projects.ErrBadKeyPrefix) {
		t.Fatalf("bad prefix: %v", err)
	}
	if err := ps.SetKeyPrefix(ctx, pid, "ky1", "TLM"); !errors.Is(err, projects.ErrKeyPrefixTaken) {
		t.Fatalf("taken prefix: %v", err)
	}
}
