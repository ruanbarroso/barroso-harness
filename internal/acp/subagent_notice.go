package acp

// Sub-agent notices over ACP. The `subagent` tool launches children in the
// background and returns at once; each result arrives later as a new turn of
// the parent. The REPL and -p already did that (AoTerminar → AvisoTerminadas →
// a turn). In -acp nobody registered AoTerminar, so the children ran and their
// results were lost: the parent never got the new turn and the client never
// learned the children had finished.
//
// Contract with the client (the Agents connector reads exactly this):
//
//   - initialize advertises agentCapabilities._meta["barroso/subagentNotices"]=true
//     when the host wired notices (HabilitarAvisosSubagentes).
//   - when sub-agents finish, the agent waits for the session's in-flight turn
//     to end, then emits a user_message_chunk with the notice text
//     ("[subagents] Sub-agents finished: ## tN <state> (<dur>) ...") and runs
//     that text as a turn of the parent on its own, streaming the usual
//     session/update notifications;
//   - that self-started turn has no session/prompt to answer, so its end is a
//     session_info_update with _meta["barroso/turnEnd"] = {"state": "completed"
//     | "failed" | "cancelled"[, "error": "..."]}.
//
// A client session/prompt that arrives during a self-started turn is queued:
// it runs as soon as that turn ends (session/cancel cancels both).

import (
	"context"
	"io"
)

// SubagentNoticesCap is the _meta key advertised in agentCapabilities.
const SubagentNoticesCap = "barroso/subagentNotices"

// SubagentActivityCap announces tagged live events from child tasks.
const SubagentActivityCap = "barroso/subagentActivity"

// SubagentActivityMeta marks a session/update as belonging to one child task.
const SubagentActivityMeta = "barroso/subagentActivity"

// TurnEndMeta is the _meta key of the session_info_update that closes a turn
// the agent started on its own.
const TurnEndMeta = "barroso/turnEnd"

// HabilitarAvisosSubagentes turns the capability on. The host calls it when it
// will call AvisarSubagentes for the sessions it creates.
func (a *Agente) HabilitarAvisosSubagentes() { a.subNotices.Store(true) }

// AvisarSubagentes tells the agent that sub-agents of sessionID finished.
// fetch returns the notice text for the tasks not yet delivered (and marks them
// delivered); ok=false means there is nothing new. Safe to call from any
// goroutine and any number of times: notices that arrive together become one
// turn, and nothing is delivered while another turn of the session is running.
func (a *Agente) AvisarSubagentes(sessionID string, fetch func() (string, bool)) {
	a.mu.Lock()
	s, ok := a.sessions[sessionID]
	if !ok || a.wire == nil {
		a.mu.Unlock()
		a.logf("acp: sub-agent notice for unknown/unwired session %s ignored", sessionID)
		return
	}
	s.fetch = fetch
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
		w := a.wire
		go a.noticeLoop(s, w)
	}
	wake := s.wake
	a.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// noticeLoop delivers the notices of one session, one self-started turn at a
// time, never overlapping a client turn.
func (a *Agente) noticeLoop(s *session, w io.Writer) {
	for range s.wake {
		for a.deliverNotice(s, w) {
		}
	}
}

// deliverNotice waits for the session to be idle, claims it and runs one
// notice turn. It reports whether a turn ran (more may have finished meanwhile).
func (a *Agente) deliverNotice(s *session, w io.Writer) bool {
	a.mu.Lock()
	for s.cancel != nil {
		done := s.idle
		a.mu.Unlock()
		if done != nil {
			<-done
		}
		a.mu.Lock()
	}
	fetch := s.fetch
	if fetch == nil {
		a.mu.Unlock()
		return false
	}
	text, ok := fetch()
	if !ok || text == "" {
		a.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.autoCancel = cancel
	s.autoIdle = make(chan struct{})
	a.mu.Unlock()

	a.running.Add(1)
	defer a.running.Done()
	rend := &acpRenderer{a: a, w: w, session: s.id}
	rend.emit("user_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": text}})
	stop := a.keepalive(rend, ctx)
	err := s.turn.Send(ctx, rend, s.cwd, text)
	stop()
	end := map[string]any{"state": "completed"}
	if err != nil {
		end["state"] = "failed"
		if ctx.Err() != nil {
			end["state"] = "cancelled"
		}
		end["error"] = errorMessage(err)
		a.logf("acp: sub-agent notice turn of session %s failed: %v", s.id, err)
	} else {
		a.logf("acp: sub-agent notice turn of session %s completed", s.id)
	}
	rend.emit("session_info_update", map[string]any{"_meta": map[string]any{TurnEndMeta: end}})

	a.mu.Lock()
	s.autoCancel = nil
	cancel()
	idle := s.autoIdle
	s.autoIdle = nil
	a.mu.Unlock()
	close(idle)
	return true
}
