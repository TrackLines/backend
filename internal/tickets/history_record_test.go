package tickets

import (
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// TL-75: every lifecycle change is recorded with the ticket change, in order, with who made it.
func TestHistoryRecorded(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'hr1'; DELETE FROM projects WHERE owner_clerk_id = 'hr1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('hr1', 'p') RETURNING id`).Scan(&pid)
	b, err := boards.Store{DB: db}.Create(ctx, "hr1", pid, "Backend", "")
	if err != nil || len(b.Columns) < 3 {
		t.Fatalf("board: %v", err)
	}
	todo, doing, done := b.Columns[0], b.Columns[1], b.Columns[len(b.Columns)-1]
	s := Store{DB: db}
	ava := clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_ava"}})
	claude := auth.WithAPIKeyUser(ctx, "user_ava", "claude")

	// trail: what was added since mark, as "stage:outcome:column:actor" (resolved shows its resolution)
	type mark int
	trail := func(id string, from mark) (string, mark) {
		t.Helper()
		h, err := s.History(ctx, "hr1", id)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, e := range h[from:] {
			where := ""
			if e.ColumnName != nil {
				where = *e.ColumnName
			}
			if e.Resolution != nil {
				where = *e.Resolution
			}
			who := "-"
			if e.Actor != nil {
				who = *e.Actor
			}
			p := e.Stage + ":" + e.Outcome + ":" + where + ":" + who
			if e.Done {
				p += ":done"
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, " "), mark(len(h))
	}
	expect := func(id string, at *mark, want string) {
		t.Helper()
		got, next := trail(id, *at)
		if got != want {
			t.Fatalf("history:\n got %s\nwant %s", got, want)
		}
		*at = next
	}

	// made in the backlog, by its creator
	bt, _ := s.CreateBacklogAs(ctx, "hr1", "user_ava", pid, "task", "from the backlog", "")
	var bm mark
	expect(bt.ID, &bm, "backlog:entered::user_ava")

	// made straight on a board: the backlog was skipped
	ot, _ := s.CreateAs(ctx, "hr1", "user_sam", todo.ID, "task", "on the board", "")
	var om mark
	expect(ot.ID, &om, "backlog:skipped::user_sam column:entered:"+todo.Name+":user_sam")
	// made in a later column: the columns before it were skipped, in order
	lt, _ := s.CreateAs(ctx, "hr1", "user_sam", done.ID, "task", "already done", "")
	var lm mark
	expect(lt.ID, &lm, "backlog:skipped::user_sam column:skipped:"+todo.Name+":user_sam column:skipped:"+doing.Name+":user_sam column:entered:"+done.Name+":user_sam:done")

	// forward over a column: it's skipped; backward: just the column
	if err := s.Move(claude, "hr1", ot.ID, done.ID, 0); err != nil {
		t.Fatal(err)
	}
	expect(ot.ID, &om, "column:skipped:"+doing.Name+":claude column:entered:"+done.Name+":claude:done")
	if err := s.Move(ava, "hr1", ot.ID, todo.ID, 0); err != nil {
		t.Fatal(err)
	}
	expect(ot.ID, &om, "column:entered:"+todo.Name+":user_ava")
	// reordering within a column isn't a change of stage
	if err := s.Move(ava, "hr1", ot.ID, todo.ID, 1); err != nil {
		t.Fatal(err)
	}
	expect(ot.ID, &om, "")

	// backlog and back: from the backlog into the second column skips the first
	if err := s.ToBacklogAs(ava, "hr1", ot.ID, false); err != nil {
		t.Fatal(err)
	}
	expect(ot.ID, &om, "backlog:entered::user_ava")
	if err := s.Move(ava, "hr1", ot.ID, doing.ID, 0); err != nil {
		t.Fatal(err)
	}
	expect(ot.ID, &om, "column:skipped:"+todo.Name+":user_ava column:entered:"+doing.Name+":user_ava")

	// resolved in the backlog (won't do): kept, readable, and reopened by moving it onto a board
	if err := s.ResolveAs(ctx, "hr1", "user_ava", bt.ID, ResolutionWontDo, ""); err != nil {
		t.Fatal(err)
	}
	expect(bt.ID, &bm, "resolved:entered:wont_do:user_ava")
	if err := s.Move(ava, "hr1", bt.ID, todo.ID, 0); err != nil {
		t.Fatal(err)
	}
	expect(bt.ID, &bm, "column:entered:"+todo.Name+":user_ava")
	// closed on its board (Bugfixes' ignore, TL-272): into Done, then resolved
	if err := s.ResolveAs(ava, "hr1", "user_ava", bt.ID, ResolutionWontDo, ""); err != nil {
		t.Fatal(err)
	}
	expect(bt.ID, &bm, "column:skipped:"+doing.Name+":user_ava column:entered:"+done.Name+":user_ava:done resolved:entered:wont_do:user_ava")

	// a duplicate on a board: into Done and resolved as a duplicate, in one change
	dt, _ := s.CreateAs(ctx, "hr1", "user_sam", doing.ID, "task", "same again", "")
	var dm mark
	_, dm = trail(dt.ID, 0)
	if err := s.MarkDuplicate(ctx, "hr1", "user_sam", dt.ID, lt.ID, DuplicateOptions{}); err != nil {
		t.Fatal(err)
	}
	expect(dt.ID, &dm, "column:entered:"+done.Name+":user_sam:done resolved:entered:duplicate:user_sam")

	// no actor (a webhook, a background job): entries without one
	if err := s.Move(ctx, "hr1", ot.ID, todo.ID, 0); err != nil {
		t.Fatal(err)
	}
	expect(ot.ID, &om, "column:entered:"+todo.Name+":-")

	// in the same transaction: a change that's rolled back leaves no history
	tx, _ := db.Begin(ctx)
	if _, err := tx.Exec(ctx, `UPDATE tickets SET column_id = $2 WHERE id = $1`, ot.ID, done.ID); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	expect(ot.ID, &om, "")

	// renaming a column later doesn't rewrite history
	_, _ = db.Exec(ctx, `UPDATE columns SET name = 'Renamed' WHERE id = $1`, todo.ID)
	if got, _ := trail(ot.ID, 0); !strings.Contains(got, "column:entered:"+todo.Name) || strings.Contains(got, "Renamed") {
		t.Fatalf("rewritten: %s", got)
	}
	// and the setting doesn't leak to the next request on the same connection
	var leaked string
	_ = db.QueryRow(ctx, `SELECT coalesce(current_setting('tracklines.actor', true), '')`).Scan(&leaked)
	if leaked != "" {
		t.Fatalf("actor leaked outside its transaction: %q", leaked)
	}
}
