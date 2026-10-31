package tools

// shell jobs. A command used to run until it ended, with no bound at
// all: a build waiting on a prompt, a server started in the foreground or a
// grandchild holding the pipe open kept the turn silent forever. The surveyed
// harnesses solve it without killing useful work, and this follows them:
//
//   - inactivity, not wall clock (Gemini CLI): a command that keeps printing
//     never dies; one silent for inactivityTimeout is killed;
//   - auto-background (Crush, Codex): a command still running after
//     autoBackground returns control to the model with the output so far, and
//     keeps running as a job the model can wait on (shell_output) or stop
//     (shell_kill);
//   - the whole process tree is killed, and Wait is bounded by waitDelay so a
//     grandchild holding the pipe cannot hang the tool (Codex's drain guard);
//   - output over outputCap is saved whole to a file and the model gets the
//     head, the tail and the path (OpenCode, Gemini, Claude Code, Goose).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Variables, not constants, so tests can shrink them.
var (
	autoBackground    = 60 * time.Second
	inactivityTimeout = 300 * time.Second
	waitDelay         = 2 * time.Second
	// defaultOutputWait / maxOutputWait bound one shell_output call.
	defaultOutputWait = 60 * time.Second
	maxOutputWait     = 10 * time.Minute
	spillRetention    = 7 * 24 * time.Hour
)

const (
	headKeep = 16 << 10
	tailKeep = outputCap - headKeep - 1<<10
)

// spillDir holds the full output of commands that went past outputCap.
func spillDir() string { return filepath.Join(os.TempDir(), "barroso-harness", "tool-output") }

var cleanOnce sync.Once

// jobOutput is the merged stdout+stderr of a job: the first headKeep bytes,
// the last tailKeep bytes, every byte in a file, and the time of the last
// write (the inactivity clock).
type jobOutput struct {
	mu    sync.Mutex
	head  []byte
	tail  []byte
	total int64
	file  *os.File
	path  string
	sink  func(string)
	last  atomic.Int64 // unix nanos of the last write
	read  int64        // bytes already shown to the model (shell_output)
}

func newJobOutput(sink func(string)) *jobOutput {
	o := &jobOutput{sink: sink}
	o.last.Store(time.Now().UnixNano())
	cleanOnce.Do(func() { go cleanSpill() })
	if err := os.MkdirAll(spillDir(), 0o755); err == nil {
		if f, err := os.CreateTemp(spillDir(), "cmd-*.log"); err == nil {
			o.file, o.path = f, f.Name()
		}
	}
	return o
}

func (o *jobOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.last.Store(time.Now().UnixNano())
	o.total += int64(len(p))
	if room := headKeep - len(o.head); room > 0 {
		o.head = append(o.head, p[:min(room, len(p))]...)
	}
	o.tail = append(o.tail, p...)
	if len(o.tail) > tailKeep {
		o.tail = append(o.tail[:0:0], o.tail[len(o.tail)-tailKeep:]...)
	}
	if o.file != nil {
		_, _ = o.file.Write(p)
	}
	if o.sink != nil && len(p) > 0 {
		o.sink(string(p))
	}
	return len(p), nil
}

func (o *jobOutput) idleFor() time.Duration {
	return time.Since(time.Unix(0, o.last.Load()))
}

// render is the output for the model. Small output comes whole and its file
// is removed; large output is head + tail + the path of the full file.
func (o *jobOutput) render(final bool) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	// Whole only when head+tail still hold every byte; past that the middle is
	// gone from memory and lives only in the file.
	if o.total <= int64(headKeep+tailKeep) {
		var s string
		if o.total <= int64(len(o.head)) {
			s = string(o.head)
		} else {
			// head and tail overlap: the tail alone holds everything after head.
			s = string(o.head) + string(o.tail[len(o.tail)-int(o.total-int64(len(o.head))):])
		}
		if final && o.file != nil {
			o.file.Close()
			os.Remove(o.path)
			o.file = nil
		}
		return s
	}
	if final && o.file != nil {
		o.file.Close()
		o.file = nil
	}
	omitted := o.total - int64(len(o.head)) - int64(len(o.tail))
	where := "the full output could not be saved"
	if o.path != "" {
		where = "full output (" + strconv.FormatInt(o.total, 10) + " bytes) saved to " + o.path + " — read it with read/grep"
	}
	return fmt.Sprintf("%s\n\n[... %d bytes omitted from the middle; %s ...]\n\n%s", o.head, omitted, where, o.tail)
}

// unread returns what the model has not seen yet (capped to the tail).
func (o *jobOutput) unread() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := o.total - o.read
	o.read = o.total
	if n <= 0 {
		return ""
	}
	if n > int64(len(o.tail)) {
		return fmt.Sprintf("[... %d earlier bytes, see %s ...]\n", n-int64(len(o.tail)), o.path) + string(o.tail)
	}
	return string(o.tail[len(o.tail)-int(n):])
}

func findProcess(pid int) (*os.Process, error) { return os.FindProcess(pid) }

func cleanSpill() {
	entries, err := os.ReadDir(spillDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > spillRetention {
			os.Remove(filepath.Join(spillDir(), e.Name()))
		}
	}
}

// job is one running (or finished) shell command.
type job struct {
	id         string
	command    string
	cmd        *exec.Cmd
	out        *jobOutput
	start      time.Time
	done       chan struct{}
	exit       int
	note       string // why it ended, when not on its own (timeout, killed)
	background bool   // started with background:true — no inactivity kill
	killOnce   sync.Once

	// Notice of the end (jobnotice.go). set is fixed before the process starts.
	set      *JobSet
	end      time.Time
	mu       sync.Mutex // guards bg, consumed, notified
	bg       bool       // runs in the background: the agent is told when it ends
	consumed bool       // the model already read the result
	notified bool
}

// code is the exit code as the model sees it.
func (j *job) code() int {
	if j.note != "" && j.exit < 0 {
		return 124
	}
	return j.exit
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*job{}
	jobSeq atomic.Int64
)

// startJob spawns command in dir. The process is NOT tied to the tool ctx:
// the caller decides when to kill it (cancellation, timeout, inactivity), so a
// job moved to the background survives the end of the call.
func startJob(command, dir string, sink func(string), set *JobSet) (*job, error) {
	cmd := terminalCmd(context.Background(), command)
	cmd.Dir = dir
	treeAttrs(cmd)
	out := newJobOutput(sink)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = waitDelay
	j := &job{
		id:      "job-" + strconv.FormatInt(jobSeq.Add(1), 10),
		command: command, cmd: cmd, out: out, start: time.Now(),
		done: make(chan struct{}), exit: -1, set: set,
	}
	if err := cmd.Start(); err != nil {
		out.render(true)
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		if cmd.ProcessState != nil {
			j.exit = cmd.ProcessState.ExitCode()
		}
		j.end = time.Now()
		close(j.done)
		j.maybeNotify()
	}()
	return j, nil
}

// kill ends the whole process tree and waits (bounded) for Wait to return.
func (j *job) kill(note string) {
	j.killOnce.Do(func() {
		if j.note == "" {
			j.note = note
		}
		if j.cmd.Process != nil {
			killTree(j.cmd.Process.Pid)
		}
	})
	select {
	case <-j.done:
	case <-time.After(waitDelay + 3*time.Second):
	}
}

func (j *job) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// result is the final text of a finished job.
func (j *job) result() string {
	code := j.exit
	if j.note != "" && code < 0 {
		code = 124 // the timeout(1) convention, as Codex reports it
	}
	s := fmt.Sprintf("exit_code: %d\nstdout+stderr:\n%s", code, j.out.render(true))
	if j.note != "" {
		s += "\n[" + j.note + "]"
	}
	return s
}

// status is the text for a job that is still running.
func (j *job) status(why string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "status: running as %s (pid %d, %s so far)\n", j.id, j.cmd.Process.Pid, time.Since(j.start).Round(time.Second))
	if why != "" {
		sb.WriteString("[" + why + "]\n")
	}
	if s := j.out.unread(); s != "" {
		sb.WriteString("output so far:\n" + s)
		if !strings.HasSuffix(s, "\n") {
			sb.WriteString("\n")
		}
	}
	if j.out.path != "" {
		sb.WriteString("full output file: " + j.out.path + "\n")
	}
	fmt.Fprintf(&sb, "Call shell_output {\"jobID\":%q} to wait for it and read new output, or shell_kill to stop it. Do something else meanwhile if you can.", j.id)
	return sb.String()
}

func register(j *job) {
	jobsMu.Lock()
	jobs[j.id] = j
	jobsMu.Unlock()
}

func lookupJob(id string) (*job, bool) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, ok := jobs[id]
	return j, ok
}

// runForeground waits for j the way shell promises: until it ends, the
// ctx is cancelled, the explicit timeout fires, it goes quiet for
// inactivityTimeout, or (when auto is set) it is moved to the background.
func runForeground(ctx context.Context, j *job, timeout time.Duration, auto bool) string {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		select {
		case <-j.done:
			return j.result()
		case <-ctx.Done():
			j.kill("cancelled: the turn was interrupted")
			return j.result()
		case <-deadline:
			j.kill(fmt.Sprintf("timed out after %s (timeout) and was killed with its child processes; output above is partial — retry with a larger timeout, or run it with background:true", timeout))
			return j.result()
		case <-tick.C:
			if inactivityTimeout > 0 && j.out.idleFor() >= inactivityTimeout {
				j.kill(fmt.Sprintf("no output for %s: killed with its child processes as stuck (it may be waiting for input — pass flags like --yes/-y, or run it with background:true if it is a server)", inactivityTimeout))
				return j.result()
			}
			if auto && autoBackground > 0 && time.Since(j.start) >= autoBackground {
				register(j)
				j.markBackground()
				go watchInactive(j)
				return j.status(fmt.Sprintf("still running after %s: moved to the background so the turn is not blocked; it keeps running", autoBackground))
			}
		}
	}
}

// watchInactive keeps the inactivity bound on a job that was moved to the
// background automatically (it was meant to finish; a quiet one is stuck).
func watchInactive(j *job) {
	if inactivityTimeout <= 0 {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-j.done:
			return
		case <-tick.C:
			if j.out.idleFor() >= inactivityTimeout {
				j.kill(fmt.Sprintf("no output for %s in the background: killed with its child processes as stuck", inactivityTimeout))
				return
			}
		}
	}
}

// waitJob is shell_output: wait up to wait for j to end, then report.
func waitJob(ctx context.Context, j *job, wait time.Duration) string {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-j.done:
	case <-t.C:
	case <-ctx.Done():
	}
	if j.finished() {
		jobsMu.Lock()
		delete(jobs, j.id)
		jobsMu.Unlock()
		j.consume()
		rest := j.out.unread()
		res := j.result()
		if len(res) > outputCap { // already shown in pieces: give the end + file
			res = fmt.Sprintf("exit_code: %d\nnew output:\n%s", j.exit, rest)
		}
		return fmt.Sprintf("%s finished after %s\n%s", j.id, time.Since(j.start).Round(time.Second), res)
	}
	return j.status("")
}

// listJobs describes the jobs still registered.
func listJobs() string {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if len(jobs) == 0 {
		return "no background jobs"
	}
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b bytes.Buffer
	for _, id := range ids {
		j := jobs[id]
		state := "running"
		if j.finished() {
			state = fmt.Sprintf("finished (exit %d)", j.exit)
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s\n", id, state, time.Since(j.start).Round(time.Second), j.command)
	}
	return b.String()
}

// KillJobs stops every background job (process shutdown).
func KillJobs() {
	jobsMu.Lock()
	list := make([]*job, 0, len(jobs))
	for _, j := range jobs {
		list = append(list, j)
	}
	jobsMu.Unlock()
	for _, j := range list {
		if !j.finished() {
			j.kill("killed: the agent is shutting down")
		}
	}
}
