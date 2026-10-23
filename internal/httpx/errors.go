package httpx

import (
	"errors"
	"net/http"
	"strings"
)

// CodedError is an error people see, with a stable code the web app translates (TL-552). The code
// never changes once shipped; the English message can.
type CodedError struct{ Code, Msg string }

func (e *CodedError) Error() string { return e.Msg }

// Code makes one: var ErrDoneParent = httpx.Code("done_parent", "a done ticket can't take sub-tickets").
func Code(code, msg string) error { return &CodedError{Code: code, Msg: msg} }

// Error answers with err and status. A client that accepts JSON (the web app) gets
// {"code":"done_parent","error":"…"}, with no code for an uncoded error (it falls back to the
// status); everyone else (the MCP, the TUI, curl) gets the plain-text message as always.
func Error(w http.ResponseWriter, r *http.Request, status int, err error) {
	if !strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Error(w, err.Error(), status)
		return
	}
	out := map[string]string{"error": err.Error()}
	if ce, ok := errors.AsType[*CodedError](err); ok {
		out["code"] = ce.Code
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	JSON(w, status, out)
}

// Coded errors more than one package answers with.
var (
	ErrNameRequired     = Code("name_required", "name is required")
	ErrTitleRequired    = Code("title_required", "title is required")
	ErrChooseOrg        = Code("choose_org", "choose an organization first")
	ErrSignInOrg        = Code("sign_in_org", "sign in to an organization")
	ErrSignIn           = Code("sign_in_required", "sign in to do this")
	ErrVerifyMembership = Code("verify_membership_failed", "could not verify organization membership")
	ErrLoadMembers      = Code("load_members_failed", "could not load organization members")
)
