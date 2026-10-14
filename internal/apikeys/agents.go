package apikeys

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// Agent keys (TL-269) say which client the agent runs in; their names are generated from it.
// Clients matches the setup steps in the frontend (components/mcp-connect.tsx).
var clientNames = map[string]string{
	"claude-code": "Claude", "claude-desktop": "Claude", "codex": "Codex", "cursor": "Cursor",
	"vscode": "Copilot", "gemini": "Gemini", "other": "",
}

// MaxLabel bounds what someone calls an "other" agent.
const MaxLabel = 40

// ValidClient: one of the clients we know (or "other").
func ValidClient(c string) bool { _, ok := clientNames[c]; return ok }

// DisplayName is what an agent key is called: the client's name, an "other" agent's label, or
// "AI agent". Its owner sees just this; everyone else sees it with the owner (Shown).
func DisplayName(client, label string) string {
	if n := clientNames[client]; n != "" {
		return n
	}
	if l := strings.TrimSpace(label); l != "" {
		return l
	}
	return "AI agent"
}

// Shown is how viewer sees an agent: DisplayName for its owner, "Claude (keloran)" for anyone else.
func Shown(display, owner, ownerHandle, viewer string) string {
	if owner == viewer || ownerHandle == "" {
		return display
	}
	return display + " (" + ownerHandle + ")"
}

// generatedName is a fresh id for an agent key: the display name's slug and six random digits.
func generatedName(client string) (string, error) {
	slug := strings.ToLower(clientNames[client])
	if slug == "" {
		slug = "agent"
	}
	n, err := rand.Int(rand.Reader, big.NewInt(900_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", slug, 100_000+n.Int64()), nil
}
