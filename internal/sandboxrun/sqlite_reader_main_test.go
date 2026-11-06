package sandboxrun

import (
	"os"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
)

func TestMain(m *testing.M) {
	if handled, code := opencodestate.DispatchSQLiteReader(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
