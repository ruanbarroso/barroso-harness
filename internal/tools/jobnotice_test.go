package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// watch returns a ctx carrying a JobSet and a counter of wake-ups.
func watch(t *testing.T) (context.Context, *JobSet, *atomic.Int32) {
	t.Helper()
	js := NewJobSet()
	var n atomic.Int32
	js.AoTerminar(func() { n.Add(1) })
	return WithJobSet(context.Background(), js), js, &n
}

func waitCount(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("wake-ups = %d, want %d", n.Load(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A background job that ends on its own is reported once, with exit and output.
func TestJobSetReportsBackgroundJob(t *testing.T) {
	shrinkJobs(t)
	ctx, js, n := watch(t)
	runTerm(t, ctx, map[string]any{"command": "echo finished-here", "background": true})
	waitCount(t, n, 1)
	fins := js.Terminadas()
	if len(fins) != 1 || fins[0].Exit != 0 || !strings.Contains(fins[0].Output, "finished-here") {
		t.Fatalf("%+v", fins)
	}
	if again := js.Terminadas(); len(again) != 0 {
		t.Fatalf("delivered twice: %+v", again)
	}
	msg := AvisoJobs(fins, 0)
	if !strings.HasPrefix(msg, "[shell] Background jobs finished:") || !strings.Contains(msg, "No background jobs running now.") {
		t.Fatal(msg)
	}
	if !regexp.MustCompile(`(?m)^## job-\d+ exit 0 \(`).MatchString(msg) {
		t.Fatal(msg)
	}
}

// A foreground command does not notify.
func TestJobSetIgnoresForeground(t *testing.T) {
	shrinkJobs(t)
	ctx, js, n := watch(t)
	runTerm(t, ctx, map[string]any{"command": "echo hi"})
	time.Sleep(200 * time.Millisecond)
	if n.Load() != 0 || len(js.Terminadas()) != 0 {
		t.Fatal("foreground job was reported")
	}
}

// A job moved to the background by the timeout is reported when it ends.
func TestJobSetAutoBackground(t *testing.T) {
	shrinkJobs(t)
	autoBackground, inactivityTimeout = 300*time.Millisecond, time.Minute
	ctx, js, n := watch(t)
	out := runTerm(t, ctx, map[string]any{"command": sleepCmd(2)})
	if !strings.Contains(out, "moved to the background") {
		t.Fatal(out)
	}
	waitCount(t, n, 1)
	if fins := js.Terminadas(); len(fins) != 1 {
		t.Fatalf("%+v", fins)
	}
}

// Reading the result with shell_output cancels the notice.
func TestJobSetConsumedByShellOutput(t *testing.T) {
	shrinkJobs(t)
	ctx, js, _ := watch(t)
	out := runTerm(t, ctx, map[string]any{"command": sleepCmd(1), "background": true})
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	b, _ := json.Marshal(map[string]any{"jobID": id, "waitSecs": 30})
	if _, err := specTerminalOutput().Exec(ctx, "", b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if fins := js.Terminadas(); len(fins) != 0 {
		t.Fatalf("reported after being read: %+v", fins)
	}
}

// Killing with shell_kill reads the result: no notice.
func TestJobSetConsumedByShellKill(t *testing.T) {
	shrinkJobs(t)
	ctx, js, _ := watch(t)
	out := runTerm(t, ctx, map[string]any{"command": sleepCmd(30), "background": true})
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	b, _ := json.Marshal(map[string]any{"jobID": id})
	if _, err := specTerminalKill().Exec(ctx, "", b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if fins := js.Terminadas(); len(fins) != 0 {
		t.Fatalf("reported after kill: %+v", fins)
	}
}

// Without a JobSet in the ctx a background job still works and nothing panics.
func TestJobSetAbsent(t *testing.T) {
	shrinkJobs(t)
	out := runTerm(t, context.Background(), map[string]any{"command": "echo x", "background": true})
	if !strings.Contains(out, "job-") {
		t.Fatal(out)
	}
	time.Sleep(300 * time.Millisecond)
}

// Two jobs that end together are two entries of one Terminadas call.
func TestJobSetBatchesJobs(t *testing.T) {
	shrinkJobs(t)
	ctx, js, n := watch(t)
	runTerm(t, ctx, map[string]any{"command": "echo a", "background": true})
	runTerm(t, ctx, map[string]any{"command": "echo b", "background": true})
	waitCount(t, n, 2)
	if fins := js.Terminadas(); len(fins) != 2 {
		t.Fatalf("%+v", fins)
	}
}
