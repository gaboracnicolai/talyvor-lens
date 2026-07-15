package main

import (
	"github.com/talyvor/lens/internal/routingbrain"
	"github.com/talyvor/lens/internal/workspace"
)

// brainWorkspaceSource adapts the workspace manager to the Routing Brain's offline
// enumeration: each workspace's id + model allow-list. Read-only; the brain never
// mutates workspaces.
type brainWorkspaceSource struct{ m *workspace.Manager }

func (s brainWorkspaceSource) BrainWorkspaces() []routingbrain.WorkspaceModels {
	wss := s.m.ListWorkspaces()
	out := make([]routingbrain.WorkspaceModels, 0, len(wss))
	for _, ws := range wss {
		out = append(out, routingbrain.WorkspaceModels{WorkspaceID: ws.ID, AllowedModels: ws.AllowedModels})
	}
	return out
}
