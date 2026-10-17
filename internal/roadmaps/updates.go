package roadmaps

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	HealthOnTrack      = "on_track"
	HealthAtRisk       = "at_risk"
	HealthOffTrack     = "off_track"
	MaxUpdateNoteRunes = 2000
)

var ErrInvalidUpdate = errors.New("health must be on_track, at_risk or off_track and note must be 1 to 2000 characters")

// Update is an immutable, dated roadmap progress report. It deliberately contains no project,
// organization, or linked-ticket fields, so the same shape is safe for public roadmap readers.
type Update struct {
	ID          string `json:"id"`
	Health      string `json:"health"`
	Note        string `json:"note"`
	PublishedAt string `json:"published_at"`
	Author      string `json:"author,omitempty"` // who posted it, by name; shown on public roadmaps too (TL-413)
}

// publicName is how an author is shown: their name, or the part of their email before the @ when
// that's all there is, since updates can be public and an email address shouldn't be.
func publicName(label string) string {
	if local, _, ok := strings.Cut(label, "@"); ok {
		return local
	}
	return label
}

func ValidUpdate(health, note string) bool {
	length := utf8.RuneCountInString(strings.TrimSpace(note))
	return (health == HealthOnTrack || health == HealthAtRisk || health == HealthOffTrack) &&
		length > 0 && length <= MaxUpdateNoteRunes
}

// CreateUpdate appends a report for the roadmap's owner. Updates cannot be edited or deleted.
// author: the poster's actor id; authorName: their name as shown (saved, so it never needs looking up).
func (s Store) CreateUpdate(ctx context.Context, roadmapID, owner, health, note, author, authorName string) (*Update, error) {
	note = strings.TrimSpace(note)
	if !ValidUpdate(health, note) {
		return nil, ErrInvalidUpdate
	}
	var out Update
	err := s.DB.QueryRow(ctx, `INSERT INTO roadmap_updates (roadmap_id, health, note, author_id, author_name)
		SELECT id, $3, $4, NULLIF($5, ''), NULLIF($6, '') FROM roadmaps WHERE id = $1 AND owner_clerk_id = $2
		RETURNING id::text, health, note, to_char(published_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), COALESCE(author_name, '')`,
		roadmapID, owner, health, note, author, publicName(authorName)).Scan(&out.ID, &out.Health, &out.Note, &out.PublishedAt, &out.Author)
	if err != nil {
		return nil, notFound(err)
	}
	return &out, nil
}

// ListUpdates returns all reports oldest first for a roadmap the caller has already been authorized to read.
func (s Store) ListUpdates(ctx context.Context, roadmapID string) ([]Update, error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text, health, note,
		to_char(published_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), COALESCE(author_name, '')
		FROM roadmap_updates WHERE roadmap_id = $1 ORDER BY published_at, id`, roadmapID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Update, error) {
		var update Update
		err := row.Scan(&update.ID, &update.Health, &update.Note, &update.PublishedAt, &update.Author)
		return update, err
	})
	if out == nil {
		out = []Update{}
	}
	return out, err
}
