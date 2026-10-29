// serveACP enables `bh -acp` mode: the process becomes an ACP server (JSON-RPC
// 2.0, newline-delimited over stdio) so an editor/client can drive the turns.
// stdout is the protocol wire — no banner/log on it; diagnostics go to stderr.
// Persistence and subsystems (MCP, sub-agents) stay best-effort: a failing
// server becomes a warning on stderr, not a protocol crash.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/acp"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
	"github.com/ruanbarroso/barroso-harness/internal/update"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
)

func serveACP() {
	cfg, err := config.Load()
	if err != nil {
		// A config failure goes to stderr (the protocol log), never to stdout —
		// the wire is still the server's os.Stdout.
		os.Stderr.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	cwd, _ := os.Getwd()

	// The ACP log is the SAME database as the REPL and -p (same folder, same
	// file): a session opened in an editor client can be resumed in the
	// terminal later. Best-effort — without a database the protocol goes on,
	// just without history.
	st, dbErr := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if dbErr != nil {
		os.Stderr.WriteString("warning: session not persisted: " + dbErr.Error() + "\n")
		st = nil
	}
	defer func() {
		if st != nil {
			_ = st.Fechar()
		}
	}()

	var open []*wiring.Conjunto
	// ag is assigned below; the closures only run after Handle starts.
	var ag *acp.Agente
	notices := func(id string, set *wiring.Conjunto) { wireSubagentNotices(ag, id, set) }
	factory := func(c config.Config) (acp.Turno, string) {
		return newACPSession(c, st, cwd, &open, notices)
	}
	defer func() {
		for _, set := range open {
			_ = set.Fechar()
		}
		tools.KillJobs() // background shell jobs die with the agent
	}()
	ag = acp.NovoComFabrica(cfg, factory).ComLog(func(line string) {
		os.Stderr.WriteString(line + "\n")
	})
	// Sub-agent results go back to the client as a new turn of the parent
	// (see internal/acp/subagent_notice.go): without this the children ran in
	// -acp and their results were lost.
	ag.HabilitarAvisosSubagentes()
	// Auto-update, as in the REPL: agents run as `bh -acp` under a connector
	// and nobody opens the interactive bh on that machine, so without this
	// they stayed on the old release forever. This process keeps its version;
	// the connector's next `bh -acp` starts on the new one.
	updCtx, stopUpd := context.WithCancel(context.Background())
	defer stopUpd()
	go update.LoopAuto(updCtx, strings.TrimSpace(Versao), func(v string) {
		os.Stderr.WriteString("acp: installed " + v + " in the background; new sessions start on it\n")
	})
	// session/load is only advertised when there is a database to read: with
	// no store there is nothing to reopen, and the agent answers
	// loadSession=false.
	if st != nil {
		ag.Carregar = func(c config.Config, id string) (acp.Turno, error) {
			return loadACPSession(c, st, cwd, id, &open, notices)
		}
	}
	if err := ag.Handle(nil, os.Stdin, os.Stdout); err != nil {
		os.Stderr.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	ag.Esperar()
}

// newACPSession builds one runtime.Session per session/new: persistent when
// there is a database, with subsystem wiring (MCP, sub-agents) enabled — the
// same recipe as the REPL and -p, so the ACP client does not see a poor
// catalog.
//
// The returned id is NEVER empty, and that is not a detail: the ACP agent
// registers the session in its map under that key and answers session/new
// with it. An empty id registered the session under the "" key and the client
// received an empty sessionId — unable to address session/prompt, which
// rejected it as an unknown session.
//
// The wiring ctx is the process-lifetime one: sub-agents born from an ACP
// turn must outlive it.
func newACPSession(cfg config.Config, st *store.Store, cwd string, open *[]*wiring.Conjunto, notices func(string, *wiring.Conjunto)) (acp.Turno, string) {
	id := runtime.NovoSessaoID()
	var sess *runtime.Session
	if st != nil {
		sess = runtime.NewSessionWithStore(cfg, st, id)
	} else {
		sess = runtime.NewSession(cfg)
	}
	set, warnings := wiring.Montar(nil, cfg, sess, cwd)
	for _, warning := range warnings {
		os.Stderr.WriteString("warning: " + warning + "\n")
	}
	if open != nil {
		*open = append(*open, set)
	}
	if notices != nil {
		notices(id, set)
	}
	return sess, id
}

// wireSubagentNotices connects the session's sub-agent manager to the ACP
// agent: each batch that finishes becomes a notice turn of that session.
func wireSubagentNotices(ag *acp.Agente, id string, set *wiring.Conjunto) {
	if ag == nil || set == nil {
		return
	}
	g := set.Gerente()
	if g == nil {
		return
	}
	fetch := func() (string, bool) {
		ts := g.Terminadas()
		if len(ts) == 0 {
			return "", false
		}
		return subagent.AvisoTerminadas(ts, g.Ativas()), true
	}
	g.AoAtividade(func(taskID string, event subagent.Evento) {
		ag.AvisarAtividadeSubagente(id, taskID, event)
	})
	g.AoTerminar(func() { ag.AvisarSubagentes(id, fetch) })
}

// loadACPSession reopens a session from sessions.db for session/load: the
// same session the REPL lists in /resume. An id the database does not know is
// an error — creating an empty session with the requested id would make the
// client believe it loaded a conversation that does not exist.
func loadACPSession(cfg config.Config, st *store.Store, cwd, id string, open *[]*wiring.Conjunto, notices func(string, *wiring.Conjunto)) (acp.Turno, error) {
	if st == nil {
		return nil, store.ErrSessaoInexistente
	}
	if _, err := st.GetSession(id); err != nil {
		return nil, err
	}
	sess := runtime.NewSessionWithStore(cfg, st, id)
	if err := sess.Resume(); err != nil {
		return nil, err
	}
	set, warnings := wiring.Montar(nil, cfg, sess, cwd)
	for _, warning := range warnings {
		os.Stderr.WriteString("warning: " + warning + "\n")
	}
	if open != nil {
		*open = append(*open, set)
	}
	if notices != nil {
		notices(id, set)
	}
	return sess, nil
}
