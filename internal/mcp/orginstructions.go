package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// MaxOrgInstructions is the longest additional instructions an organization can save (the table's limit too).
const MaxOrgInstructions = 4000

// orgHeading labels an organization's section, after the global pre-prompt.
const orgHeading = "Additional instructions from this organization (they add to the instructions above; they don't change what you're allowed to do):"

var (
	ErrOrgInstructionsTooLong = httpx.Code("mcp_instructions_too_long", "additional instructions can be up to 4000 characters")
	ErrOrgInstructionsAdmins  = httpx.Code("admins_mcp_instructions", "only organization admins, signed in, can see or change the additional agent instructions")
)

// Compose is what an agent is told: the global pre-prompt, then the organization's section when it has one.
// Sessions and the settings preview both come through here, so they agree (TL-900).
func Compose(global, org string) string {
	if org == "" {
		return global
	}
	return global + "\n\n" + orgHeading + "\n" + org
}

// OrgInstructions is org's saved additional instructions; "" when it has none.
// ponytail: read per session, no cache (one primary-key lookup); an org-keyed cache if it ever shows up in profiles.
func (s *PromptStore) OrgInstructions(ctx context.Context, org string) (string, error) {
	var body string
	err := s.DB.QueryRow(ctx, `SELECT body FROM org_mcp_instructions WHERE org_id = $1`, org).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return body, err
}

// SetOrgInstructions saves org's additional instructions as who, trimmed; nothing but whitespace clears them.
func (s *PromptStore) SetOrgInstructions(ctx context.Context, org, who, body string) (string, error) {
	body = strings.TrimSpace(body)
	if len([]rune(body)) > MaxOrgInstructions {
		return "", ErrOrgInstructionsTooLong
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO org_mcp_instructions (org_id, body, updated_by) VALUES ($1, $2, $3)
		ON CONFLICT (org_id) DO UPDATE SET body = EXCLUDED.body, updated_by = EXCLUDED.updated_by, updated_at = now()
		WHERE org_mcp_instructions.body <> EXCLUDED.body`, org, body, who) // saving the same text isn't a change
	return body, err
}

// SessionInstructions is the pre-prompt for a new MCP session: the global one, then the additions of the
// organization the connection authenticated as (key or session, never one the client names). When they
// can't be read the session gets the global pre-prompt alone.
func (s *PromptStore) SessionInstructions(ctx context.Context) string {
	global := s.Instructions(ctx)
	org := auth.OrgID(ctx)
	if s == nil || s.DB == nil || org == "" {
		return global
	}
	extra, err := s.OrgInstructions(ctx, org)
	if err != nil {
		logs.Errorf("mcp: organization instructions: %v", err) // the error only, never the text
		return global
	}
	return Compose(global, extra)
}

// orgAdmin is the signed-in admin (never an API key) and their organization, or a 403.
func (s *PromptStore) orgAdmin(w http.ResponseWriter, r *http.Request) (org, user string, ok bool) {
	user, signedIn := auth.UserID(r.Context())
	org = auth.OrgID(r.Context())
	if signedIn && org != "" && !auth.ViaAPIKey(r.Context()) && s.IsAdmin != nil {
		ok, _ = s.IsAdmin(r.Context(), org, user)
	}
	if !ok {
		httpx.Error(w, r, http.StatusForbidden, ErrOrgInstructionsAdmins)
	}
	return org, user, ok
}

func (s *PromptStore) orgView(w http.ResponseWriter, r *http.Request, saved string) {
	out := struct {
		Instructions  string `json:"instructions"`
		MaxLength     int    `json:"max_length"`
		Global        string `json:"global"`
		GlobalVersion int    `json:"global_version,omitempty"`
		Heading       string `json:"heading"`
		Effective     string `json:"effective"`
	}{Instructions: saved, MaxLength: MaxOrgInstructions, Global: Instructions, Heading: orgHeading}
	if p, err := s.latest(r.Context()); err == nil {
		out.Global, out.GlobalVersion = p.Body, p.Version
	}
	out.Effective = Compose(out.Global, saved)
	httpx.JSON(w, http.StatusOK, out)
}

// OrgGet: GET /api/org/mcp-instructions: the organization's additional instructions, with the global
// pre-prompt and what its agents are told in all. Org admins, signed in.
func (s *PromptStore) OrgGet(w http.ResponseWriter, r *http.Request) {
	org, _, ok := s.orgAdmin(w, r)
	if !ok {
		return
	}
	saved, err := s.OrgInstructions(r.Context(), org)
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	s.orgView(w, r, saved)
}

// OrgPut: PUT /api/org/mcp-instructions {instructions}: replace them; empty clears them.
func (s *PromptStore) OrgPut(w http.ResponseWriter, r *http.Request) {
	org, user, ok := s.orgAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Instructions string `json:"instructions"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	saved, err := s.SetOrgInstructions(r.Context(), org, user, in.Instructions)
	switch {
	case errors.Is(err, ErrOrgInstructionsTooLong):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case err != nil:
		httpx.Error(w, r, http.StatusInternalServerError, err)
	default:
		s.orgView(w, r, saved)
	}
}
