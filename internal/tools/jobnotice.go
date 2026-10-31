package tools

// Notice of background shell jobs. A job started with background:true (or
// moved to the background after autoBackground) used to be silent when it
// ended: the model had to poll shell_output. Now the session owns a JobSet;
// the shell tool finds it in the ctx, and when a background job ends and
// nobody consumed its result (shell_output/shell_kill), the set records it and
// calls the host's callback. The host then starts a new turn with AvisoJobs —
// the same path sub-agent notices take.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type jobSetKey struct{}

// WithJobSet returns ctx carrying the set that hears about background jobs.
func WithJobSet(ctx context.Context, s *JobSet) context.Context {
	return context.WithValue(ctx, jobSetKey{}, s)
}

func jobSetFrom(ctx context.Context) *JobSet {
	s, _ := ctx.Value(jobSetKey{}).(*JobSet)
	return s
}

// JobSet collects the background jobs of one session that finished and were
// not yet reported.
type JobSet struct {
	mu       sync.Mutex
	pending  []*job
	onFinish func()
}

// NewJobSet creates an empty set.
func NewJobSet() *JobSet { return &JobSet{} }

// AoTerminar registers who is told that jobs finished. The call carries no
// data: the receiver calls Terminadas, which delivers each job only once, so
// several jobs ending together become a single notice.
func (s *JobSet) AoTerminar(fn func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onFinish = fn
	s.mu.Unlock()
}

func (s *JobSet) add(j *job) {
	s.mu.Lock()
	s.pending = append(s.pending, j)
	fn := s.onFinish
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *JobSet) remove(j *job) {
	s.mu.Lock()
	for i, p := range s.pending {
		if p == j {
			s.pending = append(s.pending[:i:i], s.pending[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

// JobFim is a background job that ended, as the notice shows it.
type JobFim struct {
	ID      string
	Command string
	Exit    int
	Dur     time.Duration
	Note    string
	Output  string
}

// jobNoticeTail bounds the output a notice carries; the full output stays in
// the job's file (path in the text).
const jobNoticeTail = 4000

// Terminadas returns the jobs that finished and were not reported yet, in
// finish order, and forgets them.
func (s *JobSet) Terminadas() []JobFim {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	list := s.pending
	s.pending = nil
	s.mu.Unlock()
	out := make([]JobFim, 0, len(list))
	for _, j := range list {
		o := j.out.render(true)
		if len(o) > jobNoticeTail {
			o = "[... earlier output omitted ...]\n" + o[len(o)-jobNoticeTail:]
			if j.out.path != "" && j.out.total > int64(headKeep+tailKeep) {
				o += "\n(full output: " + j.out.path + ")"
			}
		}
		end := j.end
		if end.IsZero() {
			end = time.Now()
		}
		out = append(out, JobFim{
			ID: j.id, Command: j.command, Exit: j.code(), Note: j.note,
			Dur: end.Sub(j.start).Round(time.Second), Output: o,
		})
	}
	return out
}

// Ativas counts this set's background jobs still running.
func (s *JobSet) Ativas() int {
	if s == nil {
		return 0
	}
	jobsMu.Lock()
	defer jobsMu.Unlock()
	n := 0
	for _, j := range jobs {
		if j.set == s && !j.finished() {
			n++
		}
	}
	return n
}

// AvisoJobs builds the message the agent receives when background jobs end.
// The "[shell]" prefix lets the screen and the session title recognize it as
// a notice, not user speech.
func AvisoJobs(fins []JobFim, active int) string {
	var sb strings.Builder
	sb.WriteString("[shell] Background jobs finished:\n")
	for _, f := range fins {
		fmt.Fprintf(&sb, "\n## %s exit %d (%s)\n", f.ID, f.Exit, f.Dur)
		fmt.Fprintf(&sb, "command: %s\n", f.Command)
		if f.Note != "" {
			sb.WriteString("[" + f.Note + "]\n")
		}
		if strings.TrimSpace(f.Output) == "" {
			sb.WriteString("(no output)\n")
		} else {
			sb.WriteString(f.Output)
			if !strings.HasSuffix(f.Output, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	if active > 0 {
		fmt.Fprintf(&sb, "\n%d background job(s) still running; you will be told when they end.\n", active)
	} else {
		sb.WriteString("\nNo background jobs running now.\n")
	}
	return sb.String()
}

// markBackground declares that the job runs in the background: the agent
// will be told when it ends. A job that already ended is reported at once.
func (j *job) markBackground() {
	if j.set == nil {
		return
	}
	j.mu.Lock()
	j.bg = true
	j.mu.Unlock()
	if j.finished() {
		j.maybeNotify()
	}
}

// maybeNotify reports the job to its set once, if it is a background job
// nobody read yet. Both the end of the process and markBackground call it;
// the flag keeps it to one notice whichever comes second.
func (j *job) maybeNotify() {
	if j.set == nil {
		return
	}
	j.mu.Lock()
	ok := j.bg && !j.consumed && !j.notified
	if ok {
		j.notified = true
	}
	j.mu.Unlock()
	if ok {
		j.set.add(j)
	}
}

// consume marks the result as read by the model (shell_output/shell_kill):
// no notice, or the one still pending is dropped.
func (j *job) consume() {
	if j.set == nil {
		return
	}
	j.mu.Lock()
	j.consumed = true
	j.mu.Unlock()
	j.set.remove(j)
}
