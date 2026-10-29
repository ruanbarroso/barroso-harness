package acp

import "github.com/ruanbarroso/barroso-harness/internal/subagent"

// AvisarAtividadeSubagente forwards a child's conversation to the ACP client.
// Every update is tagged with its task ID, so clients can route it to the child
// transcript instead of displaying it in the parent conversation.
func (a *Agente) AvisarAtividadeSubagente(sessionID, taskID string, event subagent.Evento) {
	if taskID == "" {
		return
	}
	a.mu.Lock()
	s, ok := a.sessions[sessionID]
	if !ok || a.wire == nil {
		a.mu.Unlock()
		a.logf("acp: activity for unknown/unwired sub-agent session %s ignored", sessionID)
		return
	}
	if s.childRenderers == nil {
		s.childRenderers = make(map[string]*acpRenderer)
	}
	r := s.childRenderers[taskID]
	if r == nil {
		r = &acpRenderer{a: a, w: a.wire, session: s.id, childID: taskID, ids: make(map[string]string)}
		s.childRenderers[taskID] = r
	}
	a.mu.Unlock()

	switch event.Tipo {
	case subagent.EventoPensamento:
		r.Pensou(event.Texto, event.Duracao)
	case subagent.EventoNota:
		r.emitTagged("note", "agent_message_chunk", textContent(event.Texto))
	case subagent.EventoResposta:
		r.Resposta(event.Texto)
	case subagent.EventoToolCall:
		if event.Executando {
			r.ToolComecou(event.ID, event.Nome, event.Args)
		} else {
			r.ToolTerminou(event.ID, event.Nome, event.Args, event.Texto, event.Duracao, event.Erro)
		}
	case subagent.EventoSistema:
		r.emitTagged("system", "agent_message_chunk", textContent(event.Texto))
	}
}

func textContent(text string) map[string]any {
	return map[string]any{"content": map[string]any{"type": "text", "text": text}}
}
