package sandboxprofile

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Unsetenv("XDG_CONFIG_HOME")
	os.Exit(m.Run())
}
