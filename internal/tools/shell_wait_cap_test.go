package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// shell_output used to wait up to 600 s. A turn parked on a wait does no work
// (Zoe, 13/11: waits of 5-10 min on a saturated machine), so the cap is now
// 5 min, as in Codex. The cap is a variable the tests shrink, so this checks
// the value the harness SHIPS with, and that the spec tells the model the
// same number.
func TestShellOutputWaitCapIs300Seconds(t *testing.T) {
	if maxOutputWait != 5*time.Minute {
		t.Fatalf("maxOutputWait = %v, expected 5m", maxOutputWait)
	}
	if defaultOutputWait != time.Minute {
		t.Fatalf("defaultOutputWait = %v, expected 1m", defaultOutputWait)
	}
	spec := specTerminalOutput()
	if !strings.Contains(spec.Description, "max 300") {
		t.Errorf("description does not state the 300 s cap: %s", spec.Description)
	}
	if strings.Contains(spec.Description, "600") {
		t.Errorf("description still mentions 600: %s", spec.Description)
	}
	props := spec.Schema["properties"].(map[string]any)
	wait := props["waitSecs"].(map[string]any)["description"].(string)
	if !strings.Contains(wait, "max 300") || strings.Contains(wait, "600") {
		t.Errorf("waitSecs description = %q", wait)
	}
}

// A waitSecs above the cap is clamped to it: asking for 600 must not park the
// turn for 600 s. The cap is shrunk so the test stays fast; the clamping is
// what is under test.
func TestShellOutputClampsWaitToCap(t *testing.T) {
	shrinkJobs(t)
	old := maxOutputWait
	t.Cleanup(func() { maxOutputWait = old })
	maxOutputWait = 400 * time.Millisecond

	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(30), "background": true})
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	if id == "" {
		t.Fatal(out)
	}
	b, _ := json.Marshal(map[string]any{"jobID": id, "waitSecs": 600})
	start := time.Now()
	res, err := specTerminalOutput().Exec(context.Background(), "", b)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("waitSecs 600 waited %v, expected it clamped to the cap (400ms)", took)
	}
	if !strings.Contains(res, "status: running") {
		t.Fatalf("job should still be running after the clamped wait:\n%s", res)
	}
}

// The status of a running job must steer the agent away from parking the turn:
// do other work, read the output file, and wait only as a last resort, with
// the cap stated. The old text invited "call shell_output to wait".
func TestRunningJobStatusSteersAwayFromWaiting(t *testing.T) {
	shrinkJobs(t)
	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(30), "background": true})
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	if id == "" {
		t.Fatal(out)
	}
	j, ok := lookupJob(id)
	if !ok {
		t.Fatalf("job %s not registered", id)
	}
	st := j.status("")
	for _, want := range []string{
		"you will be told when it ends",
		"do other work now",
		"output file",
		"only when nothing else is left to do",
		"capped at 300 s",
		"shell_kill",
		id,
	} {
		if !strings.Contains(st, want) {
			t.Errorf("status lacks %q:\n%s", want, st)
		}
	}
	if strings.Contains(st, "to wait for it and read new output") {
		t.Errorf("status still invites waiting:\n%s", st)
	}
	b, _ := json.Marshal(map[string]any{"jobID": id})
	_, _ = specTerminalKill().Exec(context.Background(), "", b)
}

// The status quotes the cap that is in force, not a hard-coded number.
func TestRunningJobStatusQuotesCurrentCap(t *testing.T) {
	shrinkJobs(t)
	old := maxOutputWait
	t.Cleanup(func() { maxOutputWait = old })
	maxOutputWait = 90 * time.Second

	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(30), "background": true})
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	j, ok := lookupJob(id)
	if !ok {
		t.Fatalf("job %q not registered: %s", id, out)
	}
	if st := j.status(""); !strings.Contains(st, "capped at 90 s") {
		t.Errorf("status does not quote the current cap:\n%s", st)
	}
	b, _ := json.Marshal(map[string]any{"jobID": id})
	_, _ = specTerminalKill().Exec(context.Background(), "", b)
}
