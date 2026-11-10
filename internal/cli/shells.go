// Background shell jobs in the agents bar: the list and the live output window.
package cli

import (
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func (s *dialogSource) shells() []tools.JobVisao {
	if s == nil || s.jobs == nil {
		return nil
	}
	return s.jobs.Visao()
}

func (s *dialogSource) shellOutput(id string) (string, bool) {
	if s == nil || s.jobs == nil {
		return "", false
	}
	return s.jobs.Saida(id)
}

func (s *dialogSource) stopShell(id string) {
	if s == nil || s.jobs == nil {
		return
	}
	s.jobs.Parar(id)
}

const shellTailLines = 400

func (t *screen) openShellDialog(id string) {
	d := &dialog{title: "Shell " + id, size: dialogWide, jobID: id}
	t.updateShellDialog(d)
	t.openDialog(d)
}

// updateShellDialog rebuilds the output on every frame and keeps the view
// pinned to the bottom while the user has not scrolled away.
func (t *screen) updateShellDialog(d *dialog) {
	rows := t.dialogTextRows(d)
	atBottom := d.off >= max(0, len(d.text)-rows)
	out, ok := t.src.shellOutput(d.jobID)
	if !ok {
		d.title = "Shell " + d.jobID
		d.text = []string{"Shell not found"}
		return
	}
	running := ""
	for _, j := range t.src.shells() {
		if j.ID == d.jobID {
			running = " · running " + time.Since(j.Start).Round(time.Second).String() + " · x stops it"
		}
	}
	if running == "" {
		running = " · finished"
	}
	d.title = "Shell " + d.jobID + running
	width := max(20, min(d.size, max(20, t.w-2))-8)
	lines := strings.Split(wrap(cleanOutput(strings.TrimRight(out, "\n ")), width), "\n")
	if len(lines) > shellTailLines {
		lines = append([]string{"… (showing the last " + itoa(shellTailLines) + " lines)"}, lines[len(lines)-shellTailLines:]...)
	}
	d.text = lines
	if atBottom {
		d.off = max(0, len(d.text)-rows)
	}
}
