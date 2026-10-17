package tickets

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/tracklines/backend/internal/boards"
)

// TL-218: a parent leaving its board takes its open sub-tickets there (any depth) with it: to the
// backlog, or to the new board's first column. Finished ones and ones on other boards stay. If one
// can't move, nothing does, and the error names it.
func TestGroupMoves(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'gm1'; DELETE FROM projects WHERE owner_clerk_id = 'gm1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('gm1', 'p', 'GM') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	a, _ := bs.Create(ctx, "gm1", pid, "Backend", "")
	b, _ := bs.Create(ctx, "gm1", pid, "Web", "")
	sized, _ := bs.Create(ctx, "gm1", pid, "Sized", "")
	_ = bs.SetEstimateScale(ctx, sized.ID, "gm1", "fibonacci")
	s := Store{DB: db}
	on := func(col, title string, parent *string) string {
		tk, err := s.Create(ctx, "gm1", col, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		if parent != nil {
			if err := s.SetParent(ctx, "gm1", tk.ID, parent); err != nil {
				t.Fatal(err)
			}
		}
		return tk.ID
	}
	boardOf := func(id string) string {
		d, _ := s.Get(ctx, "gm1", id)
		if d.BoardID == nil {
			return "backlog"
		}
		return *d.BoardID
	}
	todo, done := a.Columns[0].ID, a.Columns[len(a.Columns)-1].ID
	p := on(todo, "parent", nil)
	c1 := on(todo, "open child", &p)
	g := on(a.Columns[1].ID, "grandchild", &c1)
	c2 := on(todo, "finished child", &p)
	_ = s.Move(ctx, "gm1", c2, done, 0)
	c3 := on(b.Columns[0].ID, "web's child", &p)

	// moving within the board takes nothing along
	if _, moved, err := s.MoveGroup(ctx, "gm1", p, a.Columns[1].ID, 0, nil, false, false); err != nil || len(moved) != 0 || boardOf(c1) != a.ID {
		t.Fatalf("move within the board: %v %v", moved, err)
	}

	// the preview says what would go
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil).WithContext(clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_x"}}
		c.ActiveOrganizationID = "gm1"
		return c
	}()))
	req.SetPathValue("id", p)
	NewSystem(db).GroupPreview(rec, req)
	var preview []Moved
	_ = json.Unmarshal(rec.Body.Bytes(), &preview)
	if rec.Code != 200 || len(preview) != 2 || preview[0].ID != c1 || preview[1].ID != g {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}

	// to the backlog: the open child and grandchild come; the finished one and Web's stay
	moved, err := s.ToBacklogGroup(ctx, "gm1", p, false)
	if err != nil || len(moved) != 2 || moved[0].ID != c1 || moved[1].ID != g || moved[0].Key == nil {
		t.Fatalf("to backlog: %+v %v", moved, err)
	}
	for id, want := range map[string]string{p: "backlog", c1: "backlog", g: "backlog", c2: a.ID, c3: b.ID} {
		if got := boardOf(id); got != want {
			t.Errorf("%s on %s, want %s", id, got, want)
		}
	}

	// to another board: the sub-tickets go to its first column
	q := on(todo, "moving parent", nil)
	d1 := on(a.Columns[1].ID, "its part", &q)
	_, moved, err = s.MoveGroup(ctx, "gm1", q, b.Columns[1].ID, 0, nil, false, false)
	if err != nil || len(moved) != 1 || moved[0].ID != d1 {
		t.Fatalf("to another board: %+v %v", moved, err)
	}
	if d, _ := s.Get(ctx, "gm1", d1); d.BoardID == nil || *d.BoardID != b.ID || d.ColumnID == nil || *d.ColumnID != b.Columns[0].ID {
		t.Fatalf("sub-ticket landed in %v / %v", d.BoardID, d.ColumnID)
	}

	// refused for one, refused for all: a board that needs estimates won't take an unsized sub-ticket
	r := on(todo, "sized parent", nil)
	r1 := on(todo, "unsized part", &r)
	five := "5"
	_, _, err = s.MoveGroup(ctx, "gm1", r, sized.Columns[0].ID, 0, &five, true, false)
	var group GroupMoveError
	var unestimated EstimateRequiredError
	if !errors.As(err, &group) || group.Sub.ID != r1 || !errors.As(err, &unestimated) || !strings.Contains(err.Error(), "GM-") {
		t.Fatalf("refusal: %v", err)
	}
	if boardOf(r) != a.ID || boardOf(r1) != a.ID {
		t.Fatal("part of a refused move happened")
	}

	// the API says what came along
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/", nil).WithContext(req.Context())
	u := on(todo, "api parent", nil)
	on(todo, "api part", &u)
	req.SetPathValue("id", u)
	NewSystem(db).ToBacklog(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"also_moved"`) || !strings.Contains(rec.Body.String(), "api part") {
		t.Fatalf("send to backlog: %d %s", rec.Code, rec.Body)
	}
}
