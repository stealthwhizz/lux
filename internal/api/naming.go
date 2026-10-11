package api

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/store"
)

// Agents made with one click are called "New agent" (or "New agent 2"…)
// until their first job names them.
var placeholderName = regexp.MustCompile(`(?i)^new agent( [0-9]+)?$`)

const placeholderDescription = "Your personal agent"

var nameWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9'&-]*$`)

// cleanName turns a model's answer into an agent name of one or two words,
// or "" if it is not one.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	s = strings.Trim(s, "\"'`*.:!# ")
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "Name:"), "name:"))
	words := strings.Fields(s)
	if len(words) == 0 || len(words) > 2 || len(s) > 24 {
		return ""
	}
	for i, w := range words {
		if !nameWord.MatchString(w) {
			return ""
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	name := strings.Join(words, " ")
	if placeholderName.MatchString(name) {
		return ""
	}
	return name
}

// nameFromFirstJob names a one-click agent after the first thing it is
// asked to do, using the agent's own model. It runs in the background and
// leaves the agent alone if anyone renamed it meanwhile or the model's
// answer is not a usable name.
func (s *Server) nameFromFirstJob(agent *store.Agent, job string) {
	if s.Gateway == nil || agent.Revision == nil || !placeholderName.MatchString(agent.Name) || agent.SourceKind != agentrepo.SourceManaged {
		return
	}
	var report agentrepo.Report
	if json.Unmarshal(agent.Revision.Validation, &report) != nil || len(report.Models) == 0 {
		return
	}
	ref := report.Models[0]
	p, ok := s.Gateway.Table()[ref.Provider]
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		answer, err := s.Gateway.Ask(ctx, p, ref.Model,
			"You name personal assistants after the job they do, like Calendar Planner, Inbox Helper or Trip Planner. "+
				"Never a person's name. Reply with the name only: one or two words, Title Case, no punctuation.",
			"Name an assistant whose first job is: "+oneLine(job, 500))
		name := cleanName(answer)
		if err != nil || name == "" {
			s.Log.Info("agent keeps its placeholder name", "agent", agent.ID, "err", err)
			return
		}
		now, err := s.Store.GetAgent(ctx, agent.WorkspaceID, agent.ID)
		if err != nil || now.Name != agent.Name {
			return
		}
		desc := now.Description
		if desc == placeholderDescription {
			desc = oneLine(job, 300)
		}
		if err := s.Store.SetAgentProfile(ctx, agent.ID, name, desc); err != nil {
			s.Log.Error("name agent", "agent", agent.ID, "err", err)
			return
		}
		s.Log.Info("agent named after its first job", "agent", agent.ID, "name", name)
	}()
}

// oneLine collapses whitespace and cuts at a word boundary.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndex(s[:max-3], " ")
	if cut <= 0 {
		cut = max - 3
	}
	return s[:cut] + "..."
}
