package cli

import (
	"os"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// The plain renderer reads BH_FINAL_ONLY once, when it is built. An inherited
// value would change what the tests capture (and what a real user's `-p` run
// prints), so the suite runs with it unset; tests that need it set it.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(runtime.EnvSoResposta)
	os.Exit(m.Run())
}
