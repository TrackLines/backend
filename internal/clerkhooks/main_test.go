package clerkhooks

import (
	"os"
	"testing"

	"github.com/tracklines/backend/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }
