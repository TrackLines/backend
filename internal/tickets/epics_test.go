package tickets

import (
	"errors"
	"testing"

	"github.com/tracklines/backend/internal/boards"
)

// TL-213: epics live in the backlog, take no estimate, never nest, and get progress and size from
// their sub-tickets. Any ticket can become one; one on a board goes to the backlog.
func TestEpics(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ep1'; DELETE FROM projects WHERE owner_clerk_id = 'ep1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('ep1', 'p', 'EP') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	fib, _ := bs.Create(ctx, "ep1", pid, "Backend", "")
	_ = bs.SetEstimateScale(ctx, fib.ID, "ep1", "fibonacci")
	shirt, _ := bs.Create(ctx, "ep1", pid, "Web", "")
	_ = bs.SetEstimateScale(ctx, shirt.ID, "ep1", "tshirt")
	todo, done := fib.Columns[0].ID, fib.Columns[len(fib.Columns)-1].ID
	s := Store{DB: db}
	backlog := func(typ, title string) string {
		tk, err := s.CreateBacklog(ctx, "ep1", pid, typ, title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	sized := func(col, title, est string) string {
		tk, err := s.CreateEstimated(ctx, "ep1", "u", col, "task", title, "", est)
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	parent := func(child, p string) error { return s.SetParent(ctx, "ep1", child, &p) }

	// epics are made in the backlog, and stay there
	if _, err := s.Create(ctx, "ep1", todo, "epic", "on a board", ""); !errors.Is(err, ErrEpicOffBoard) {
		t.Fatalf("epic made on a board: %v", err)
	}
	epic := backlog("epic", "Checkout")
	if err := s.Move(ctx, "ep1", epic, todo, 0); !errors.Is(err, ErrEpicOffBoard) {
		t.Fatalf("epic moved onto a board: %v", err)
	}
	if err := s.SetEstimate(ctx, "ep1", epic, "5"); !errors.Is(err, ErrEpicEstimate) {
		t.Fatalf("epic estimated: %v", err)
	}
	if err := s.SetEstimate(ctx, "ep1", epic, ""); err != nil {
		t.Fatalf("clearing an epic's (absent) estimate: %v", err)
	}

	// work joins an epic as its sub-tickets, from any board; size adds their estimates (T-shirt as points)
	a, b, c := sized(todo, "a", "3"), sized(done, "b", "5"), sized(shirt.Columns[0].ID, "c", "M")
	d := backlog("task", "d, not sized yet")
	dup := sized(todo, "dup", "8")
	for _, id := range []string{a, b, c, d, dup} {
		if err := parent(id, epic); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkDuplicate(ctx, "ep1", "u", dup, a, DuplicateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "ep1", epic)
	if err != nil || got.Epic == nil {
		t.Fatalf("epic detail: %+v %v", got, err)
	}
	if p := *got.Epic; p.Done != 1 || p.Total != 4 || p.Points != 11 || p.Unestimated != 1 {
		t.Fatalf("progress: %+v (want 1 of 4 done, 3+5+M(3) = 11 points, 1 unestimated)", p)
	}
	// its sub-tickets say where they are, so the epic's page can group them by board (TL-214)
	where := map[string]string{}
	for _, ch := range got.Children {
		if ch.BoardName != nil {
			where[ch.ID] = *ch.BoardName
		}
		if ch.ID == a && (ch.Key == nil || ch.Estimate == nil || *ch.Estimate != "3") {
			t.Fatalf("child: %+v", ch)
		}
	}
	if where[a] != "Backend" || where[c] != "Web" || where[d] != "" {
		t.Fatalf("children's boards: %v", where)
	}
	if plain, _ := s.Get(ctx, "ep1", a); plain.Epic != nil {
		t.Fatal("a task got epic progress")
	}

	// no epic inside another, however deep
	other := backlog("epic", "Search")
	if err := parent(other, epic); !errors.Is(err, ErrEpicInEpic) {
		t.Fatalf("epic in an epic: %v", err)
	}
	if err := parent(other, a); !errors.Is(err, ErrEpicInEpic) {
		t.Fatalf("epic under an epic's sub-ticket: %v", err)
	}
	holder := backlog("feature", "holds an epic")
	_ = parent(other, holder)
	if err := parent(holder, epic); !errors.Is(err, ErrEpicInEpic) {
		t.Fatalf("ticket holding an epic put in an epic: %v", err)
	}

	// any ticket can become an epic: a parent on a board goes to the backlog, sized no more; its sub-tickets stay put
	feature := sized(todo, "big feature", "13")
	kid := sized(todo, "part of it", "2")
	_ = parent(kid, feature)
	if err := s.Update(ctx, "ep1", feature, "epic", "big feature", "", ""); err != nil {
		t.Fatal("convert:", err)
	}
	conv, _ := s.Get(ctx, "ep1", feature)
	if conv.Type != "epic" || conv.BoardID != nil || conv.Estimate != nil || conv.Epic == nil || conv.Epic.Total != 1 {
		t.Fatalf("converted: type %s board %v estimate %v epic %+v", conv.Type, conv.BoardID, conv.Estimate, conv.Epic)
	}
	if k, _ := s.Get(ctx, "ep1", kid); k.BoardID == nil || *k.BoardID != fib.ID {
		t.Fatal("its sub-ticket left the board")
	}
	// ...but not one inside an epic, or holding one
	if err := s.Update(ctx, "ep1", a, "epic", "a", "", ""); !errors.Is(err, ErrEpicInEpic) {
		t.Fatalf("an epic's sub-ticket made an epic: %v", err)
	}
	if err := s.Update(ctx, "ep1", holder, "epic", "holds an epic", "", ""); !errors.Is(err, ErrEpicInEpic) {
		t.Fatalf("a ticket holding an epic made an epic: %v", err)
	}
	if h, _ := s.Get(ctx, "ep1", holder); h.Type != "feature" {
		t.Fatal("refused conversion still changed the type")
	}

	// filter by epic (TL-215): tickets say which epic they're in, at any depth; the backlog and search narrow to one
	grandchild := backlog("task", "deeper")
	_ = parent(grandchild, d)
	for id, want := range map[string]string{a: epic, d: epic, grandchild: epic, kid: feature, holder: ""} {
		tk, _ := s.Get(ctx, "ep1", id)
		if got := ""; tk.EpicID != nil {
			got = *tk.EpicID
			if got != want {
				t.Fatalf("%s in epic %s, want %q", tk.Title, got, want)
			}
		} else if want != "" {
			t.Fatalf("%s: no epic, want %s", tk.Title, want)
		}
	}
	page, err := s.PageBacklogIn(ctx, "ep1", pid, epic, "", nil, 1, 25, "")
	if err != nil || len(page.Tickets) != 2 || page.Counts["all"] != 2 { // d and its sub-ticket; a, b and c are on boards
		t.Fatalf("backlog in the epic: %+v %v", page, err)
	}
	found, err := s.Search(ctx, "ep1", SearchQuery{ProjectID: pid, Epic: epic, Limit: 50})
	if err != nil || len(found) != 6 { // a, b, c, d, the duplicate (still a sub-ticket) and d's sub-ticket
		t.Fatalf("search in the epic: %d %v", len(found), err)
	}

	// the project's epics: open first, with progress; finished ones after; nobody else's
	finished := backlog("epic", "Done already")
	if err := s.Resolve(ctx, "ep1", finished); err != nil {
		t.Fatal(err)
	}
	list, err := s.Epics(ctx, "ep1", pid)
	if err != nil || len(list) != 4 || list[3].ID != finished || !list[3].Done {
		t.Fatalf("epics: %+v %v", list, err)
	}
	for _, e := range list {
		if e.ID == epic && (e.Progress.Total != 4 || e.Key == nil || *e.Key == "") {
			t.Fatalf("listed progress: %+v", e)
		}
	}
	if _, err := s.Epics(ctx, "someone-else", pid); !errors.Is(err, ErrNotFound) {
		t.Fatal("another organization listed them:", err)
	}
}
