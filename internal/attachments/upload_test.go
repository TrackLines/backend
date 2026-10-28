package attachments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// fakeStore is UploadThing: it keeps what was uploaded and deleted; bad makes the next URLs not match their keys.
type fakeStore struct {
	mu       sync.Mutex
	uploaded []string
	deleted  []string
	bad      bool
}

func (f *fakeStore) Upload(_ context.Context, name, _ string, _ []byte) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := "up-" + strconv.Itoa(len(f.uploaded))
	f.uploaded = append(f.uploaded, key)
	if f.bad {
		return key, "https://app.ufs.sh/f/not-" + key, nil
	}
	return key, "https://app.ufs.sh/f/" + key, nil
}

func (f *fakeStore) DeleteFiles(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, keys...)
	return nil
}

func (f *fakeStore) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploaded), len(f.deleted)
}

// Upload (TL-659): API-key clients send the file itself; the ticket is checked before anything is
// stored, limits hold under concurrent uploads, and a file that can't be recorded is deleted again.
// Delete: only whoever uploaded a file, or an org admin (people only, not their agents).
func TestUploadAndRemove(t *testing.T) {
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
	const org, other = "org_up1", "org_up2"
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('org_up1', 'org_up2')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Uploads') RETURNING id`, org).Scan(&pid)
	ticket := func(title string, done bool) string {
		var id string
		q := `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', $2, 0) RETURNING id`
		if done {
			q = `INSERT INTO tickets (project_id, created_by, title, position, resolved_at) VALUES ($1, 'x', $2, 0, now()) RETURNING id`
		}
		if err := db.QueryRow(ctx, q, pid, title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	open, done := ticket("open", false), ticket("done", true)

	files := &fakeStore{}
	h := NewSystem(db)
	h.IsAdmin = func(_ context.Context, _, user string) (bool, error) { return user == "user_admin", nil }
	h.Files = files

	agent := func(o, owner, name string) context.Context { return auth.WithAPIKey(ctx, owner, o, name, "ai") }
	person := func(user string) context.Context { return auth.WithAPIKey(ctx, user, org, "", "personal") }
	ava := agent(org, "user_ava", "Ava")
	upload := func(c context.Context, id, query, contentType, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/?"+query, strings.NewReader(body)).WithContext(c)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Accept", "application/json")
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.Upload(rec, req)
		return rec
	}
	expect := func(rec *httptest.ResponseRecorder, want int, has string) {
		t.Helper()
		if rec.Code != want || !strings.Contains(rec.Body.String(), has) {
			t.Fatalf("got %d %s, want %d with %q", rec.Code, rec.Body, want, has)
		}
	}
	nothingStored := func() {
		t.Helper()
		if up, _ := files.counts(); up != 0 {
			t.Fatalf("stored %d files before refusing", up)
		}
	}

	// refused before anything is stored
	expect(upload(ava, open, "", "text/plain", "hi"), 400, "bad_upload")
	expect(upload(ava, open, "name=..%2F", "text/plain", "hi"), 400, "bad_upload")
	expect(upload(ava, open, "name=a.txt", "not a type;;", "hi"), 400, "bad_upload")
	expect(upload(ava, done, "name=a.txt", "text/plain", "hi"), 409, "done_ticket_files")
	expect(upload(agent(other, "user_ava", "Ava"), open, "name=a.txt", "text/plain", "hi"), 404, "")
	big := strings.Repeat("x", MaxFileBytes+1)
	expect(upload(ava, open, "name=big.bin", "", big), 400, "file_too_big") // Content-Length says so
	req := httptest.NewRequest("POST", "/?name=big.bin", struct{ *strings.Reader }{strings.NewReader(big)}).WithContext(ava)
	req.ContentLength = -1 // streamed, no length: cut off at the limit
	req.Header.Set("Accept", "application/json")
	req.SetPathValue("id", open)
	rec := httptest.NewRecorder()
	h.Upload(rec, req)
	expect(rec, 400, "file_too_big")
	nothingStored()
	off := NewSystem(db)
	rec = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/?name=a.txt", strings.NewReader("hi")).WithContext(ava)
	r.Header.Set("Accept", "application/json")
	r.SetPathValue("id", open)
	off.Upload(rec, r)
	expect(rec, 503, "uploads_unavailable")

	// stored and recorded: folders dropped from the name, no type means octet-stream
	rec = upload(ava, open, "name="+strings.ReplaceAll(`C:\logs\run.log`, `\`, "%5C"), "", "log lines")
	expect(rec, 201, `"name":"run.log"`)
	expect(rec, 201, `"content_type":"application/octet-stream"`)
	expect(rec, 201, `"created_by":"Ava"`)
	expect(upload(ava, open, "name=shot.png", "image/png", "png"), 201, `"size":3`)
	list, err := h.store.List(ctx, org, open)
	if err != nil || len(list) != 2 {
		t.Fatalf("listed %+v %v", list, err)
	}

	// recording fails after the upload: the file is deleted again
	files.bad = true
	expect(upload(ava, open, "name=c.txt", "text/plain", "c"), 400, "bad_file")
	files.bad = false
	if up, del := files.counts(); up != 3 || del != 1 || files.deleted[0] != "up-2" {
		t.Fatalf("uploaded %d, deleted %v", up, files.deleted)
	}

	// ten at once onto a ticket with room for one: one gets in, the rest are refused and their files deleted
	full := ticket("nearly full", false)
	for i := range MaxPerTicket - 1 {
		k := "pre-" + strconv.Itoa(i)
		if _, err := h.store.Create(ctx, org, "x", full, Attachment{Key: k, URL: "https://app.ufs.sh/f/" + k, Name: k}); err != nil {
			t.Fatal(err)
		}
	}
	before, beforeDel := files.counts()
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Go(func() { codes[i] = upload(ava, full, "name=race.txt", "text/plain", "r").Code })
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == 201 {
			created++
		} else if c != 409 {
			t.Fatalf("codes %v", codes)
		}
	}
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_attachments WHERE ticket_id = $1`, full).Scan(&n)
	up, del := files.counts()
	if created != 1 || n != MaxPerTicket || (up-before)-(del-beforeDel) != 1 {
		t.Fatalf("created %d, %d on the ticket, uploaded %d, deleted %d", created, n, up-before, del-beforeDel)
	}
	expect(upload(ava, full, "name=more.txt", "text/plain", "m"), 409, "too_many_files")

	// and deterministically: an add waits while another holds the ticket, then counts what that one added
	last := ticket("last place", false)
	for i := range MaxPerTicket - 1 {
		k := "lp-" + strconv.Itoa(i)
		if _, err := h.store.Create(ctx, org, "x", last, Attachment{Key: k, URL: "https://app.ufs.sh/f/" + k, Name: k}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) // a failure below mustn't leave the pool waiting on it
	if err := room(ctx, tx, org, last, true); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := h.store.Create(ctx, org, "x", last, Attachment{Key: "lp-late", URL: "https://app.ufs.sh/f/lp-late", Name: "late"})
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("didn't wait for the ticket: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, size_bytes, content_type, created_by)
		VALUES ($1, 'lp-tenth', 'https://app.ufs.sh/f/lp-tenth', 'tenth', 1, '', 'x')`, last); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; !errors.Is(err, ErrTooMany) {
		t.Fatalf("eleventh: %v", err)
	}

	// removing: whoever uploaded it, or an org admin in person; not another agent, a member, or an admin's agent
	remove := func(c context.Context, id string) int {
		r := httptest.NewRequest("DELETE", "/", nil).WithContext(c)
		r.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.Delete(rec, r)
		return rec.Code
	}
	for who, c := range map[string]context.Context{
		"another agent": agent(org, "user_ava", "Bob"), "a member": person("user_x"), "an admin's agent": agent(org, "user_admin", "Cleo"),
	} {
		if got := remove(c, list[0].ID); got != http.StatusForbidden {
			t.Fatalf("%s removed it: %d", who, got)
		}
	}
	if got := remove(ava, list[0].ID); got != http.StatusNoContent {
		t.Fatalf("uploader: %d", got)
	}
	if got := remove(person("user_admin"), list[1].ID); got != http.StatusNoContent {
		t.Fatalf("admin: %d", got)
	}

	// TL-712: the person who owns the agent key removes its files, signed in or with their personal key;
	// not while someone else in the org has an agent key by the same name
	if _, err := db.Exec(ctx, `DELETE FROM api_keys WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO api_keys (owner_clerk_id, org_id, name, kind, prefix, hash) VALUES
			('user_ava', $1, 'Ava', 'ai', 'tl_a', 'h-up-ava'), ('user_ava', $1, 'Dup', 'ai', 'tl_a', 'h-up-dup1'), ('user_x', $1, 'Dup', 'ai', 'tl_a', 'h-up-dup2')`, org); err != nil {
		t.Fatal(err)
	}
	expect(upload(ava, open, "name=mine.txt", "text/plain", "m"), 201, `"created_by":"Ava"`)
	dup := agent(org, "user_ava", "Dup")
	expect(upload(dup, open, "name=dup.txt", "text/plain", "d"), 201, `"created_by":"Dup"`)
	byName := map[string]string{}
	all, _ := h.store.List(ctx, org, open)
	for _, a := range all {
		byName[a.Name] = a.ID
	}
	if got := remove(person("user_x"), byName["mine.txt"]); got != http.StatusForbidden {
		t.Fatalf("someone else removed the agent's file: %d", got)
	}
	if got := remove(agent(org, "user_ava", "Bob"), byName["mine.txt"]); got != http.StatusForbidden {
		t.Fatalf("the owner's other agent removed it: %d", got)
	}
	if got := remove(person("user_ava"), byName["dup.txt"]); got != http.StatusForbidden {
		t.Fatalf("a name two people's keys share counted as owned: %d", got)
	}
	if got := remove(person("user_ava"), byName["mine.txt"]); got != http.StatusNoContent {
		t.Fatalf("the agent key's owner: %d", got)
	}
}
