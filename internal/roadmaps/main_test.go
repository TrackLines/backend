package roadmaps

import (
	"context"
	"os"
	"testing"

	"github.com/tracklines/backend/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

// everyone lets any caller change roadmaps, for tests about something else; the real rule (TL-413)
// is tested in managers_test.go.
type everyone struct{}

func (everyone) IsProjectManager(context.Context, string, string, string) (bool, error) {
	return true, nil
}
