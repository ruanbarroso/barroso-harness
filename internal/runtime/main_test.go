package runtime

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// bhHelperFlag makes the package's own test binary act as a prompt hook, so
// the hook tests need no shell and run the same on Windows and POSIX.
// Each following argument is one step:
//
//	stdin:PATH  copy stdin to PATH
//	out:TEXT    write TEXT and a newline to stdout
//	err:TEXT    write TEXT and a newline to stderr
//	sleep:MS    sleep MS milliseconds
//	spawn:MS    start a grandchild that sleeps MS ms holding stdout/stderr
//	exit:N      exit with code N
const bhHelperFlag = "-bh-hook-helper"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == bhHelperFlag {
		os.Exit(bhHelperMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func bhHelperMain(steps []string) int {
	for _, st := range steps {
		op, arg, _ := strings.Cut(st, ":")
		switch op {
		case "stdin":
			b, _ := io.ReadAll(os.Stdin)
			if err := os.WriteFile(arg, b, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "out":
			fmt.Fprintln(os.Stdout, arg)
		case "err":
			fmt.Fprintln(os.Stderr, arg)
		case "sleep":
			ms, _ := strconv.Atoi(arg)
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "spawn":
			c := exec.Command(os.Args[0], bhHelperFlag, "sleep:"+arg)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case "exit":
			n, _ := strconv.Atoi(arg)
			return n
		default:
			fmt.Fprintln(os.Stderr, "unknown helper step:", st)
			return 1
		}
	}
	return 0
}

// helperHook is a prompt hook that runs this test binary with the steps above.
func helperHook(t *testing.T, timeout int, steps ...string) config.PromptHook {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.PromptHook{Command: exe, Args: append([]string{bhHelperFlag}, steps...), Timeout: timeout}
}
