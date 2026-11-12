// Package workflow is the unified worklist (assembly protocol 09, D68): the task projections the deployment's chosen
// flow managers publish by the shared task contract, shown to their candidates, with the action entry of each task's
// own component; it keeps no decision and forwards no action.
package workflow

import (
	"embed"
	"io/fs"

	core "github.com/brickKit/infra-workflow/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/component"
	"github.com/brickKit/be-sdk-go/v3/config"
	"github.com/brickKit/be-sdk-go/v3/runtime"
)

// Identity.
const (
	Version  = "3.0.4"
	GRPCPort = 9201
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrations() fs.FS {
	sub, _ := fs.Sub(migrationFiles, "migrations")
	return sub
}

// Keys are the component's own configuration keys.
func Keys() []config.Key {
	return []config.Key{{Name: core.KeySources, Type: config.String}, {Name: core.KeyStaleAfter, Type: config.Duration, Default: core.StaleAfter}}
}

// Own describes the own keys for the manifest's configSchema.
var Own = map[string]string{
	core.KeySources: "the chosen publisher of each task type, and its previous ones: <task type>=<component>[|<previous " +
		"component>...], comma separated (e.g. erp.purchase.order.approval=erp/purchase); unset: no task type",
	core.KeyStaleAfter: "a duration (e.g. 15m): how long without a heartbeat or a projection a publisher's tasks may not " +
		"be current; unset: 15m",
}

// Definition is the component as the launcher and the shells take it.
func Definition() component.Definition {
	return component.Definition{ID: core.ID, Version: Version, Keys: Keys(), Migrations: migrations(), GRPCPort: GRPCPort,
		Subscribes: true, Authz: core.Definitions(), Recovery: core.OpRecover,
		New: func(rt *runtime.Runtime) (component.Module, error) { return New(rt) }}
}
