package workflow

import (
	"context"
	"time"

	core "github.com/brickKit/infra-workflow/v3/internal/core"
	projections "github.com/brickKit/infra-workflow/v3/internal/projections"
	sources "github.com/brickKit/infra-workflow/v3/internal/sources"
	worklist "github.com/brickKit/infra-workflow/v3/internal/worklist"

	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	"github.com/brickKit/be-sdk-go/v3/work"
)

// Module is the worklist as the launcher and the shells run it: it only assembles the business modules under
// internal/ (their map is internal/AGENTS.md).
type Module struct {
	b *core.Base
}

// New builds the module from its runtime: the deployment's chosen publishers (a malformed configuration stops the
// member, WF08) and the stale threshold.
func New(rt *runtime.Runtime) (*Module, error) {
	s, err := sources.Parse(rt.Config.String(core.KeySources))
	if err != nil {
		return nil, err
	}
	return &Module{b: &core.Base{RT: rt, Sources: s, StaleAfter: rt.Config.Duration(core.KeyStaleAfter)}}, nil
}

// Routes registers the user surface (contract/openapi.yaml).
func (m *Module) Routes(r *httpx.Routes) error {
	h := func(f func(*core.Base, *httpx.Context) error) httpx.Handler {
		return func(c *httpx.Context) error { return f(m.b, c) }
	}
	r.Add(httpx.Route{Method: "GET", Path: "/api/workflow/tasks", Operation: core.OpTaskRead, Handle: h(worklist.Mine)})
	r.Add(httpx.Route{Method: "GET", Path: "/api/workflow/tasks/done", Operation: core.OpTaskRead, Handle: h(worklist.Done)})
	r.Add(httpx.Route{Method: "GET", Path: "/api/workflow/tasks/:id", Operation: core.OpTaskRead, Handle: h(worklist.Get)})
	r.Add(httpx.Route{Method: "GET", Path: "/api/workflow/rejected-projections", Operation: core.OpAdmin,
		Handle: h(projections.Rejected)})
	return nil
}

// Subscriptions are the chosen publishers' streams (WF08): their task projections and heartbeats.
func (m *Module) Subscriptions(s *events.Subscriptions) error {
	if m.b == nil { // the contract tests build the module without a runtime
		return nil
	}
	for _, sub := range projections.Subs(m.b, projections.OnEvent) {
		s.Add(sub)
	}
	return nil
}

// Jobs declares the background work (protocol 01): the purge of the refused projections.
func (m *Module) Jobs(j *work.Jobs) error {
	j.Add(work.Job{Name: projections.JobPurge, Kind: work.Singleton, Effect: work.Local, Interval: 24 * time.Hour,
		MaxAttempts: work.Unlimited, Handle: func(ctx context.Context, r *work.Run) error { return projections.Purge(m.b, ctx, r) }})
	return nil
}
