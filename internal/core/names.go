// Package core is what every module of the worklist uses: its names, its resources and operations, its errors and
// checks, and Base.
package core

import (
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
)

// ID is the component's ID.
const ID = "infra/workflow"

// The configuration (docs/design.md WF06, WF08).
const (
	// KeySources lists, for each task type, its publisher and previous publishers:
	// <task type>=<component>[|<previous component>...], comma separated.
	KeySources = "WORKFLOW_TASK_SOURCES"
	// KeyStaleAfter is how long without a heartbeat or a projection a publisher's tasks may not be current.
	KeyStaleAfter = "WORKFLOW_STALE_AFTER"
	// StaleAfter is its starting value: three heartbeats (be-protocol P17.18).
	StaleAfter = 15 * time.Minute
)

// Resources and operations (contract/openapi.yaml names each route's operation).
const (
	Tasks       = "infra.workflow.task"
	OpTaskRead  = "infra.workflow.task.read" // granted to every user: the candidates bound what one sees (WF03)
	Projections = "infra.workflow.projection"
	OpAdmin     = "infra.workflow.projection.read" // the refused projections (WF01): resource and action, as every key
	System      = "infra.workflow.system"
	// OpRecover is a full recovery's checks (be-protocol chapter 14: the recovery tool only).
	OpRecover = "infra.workflow.system.recover"
)

// Event type suffixes of a publisher's stream (be-protocol P17.15, P17.18): <stream>_task.changed.v1 and
// <stream>_task.heartbeat.v1.
const (
	TaskChangedSuffix   = "_task.changed.v1"
	TaskHeartbeatSuffix = "_task.heartbeat.v1"
)

// The reasons a projection is refused (WF01, WF09; the contract's rejected projection).
const (
	NotConfigured = "task_type_not_configured"
	NotChosen     = "not_the_chosen_publisher"
	Malformed     = "malformed"
)

// Parameters.
const (
	DefaultLimit = 50
	MaxLimit     = 200
	// RejectedKept is how long a refused projection is kept (docs/design.md, the data).
	RejectedKept = 90 * 24 * time.Hour
)

// Definitions are the component's resources and operations. The tasks have no dimension: who sees a task is who its
// publisher made a candidate (WF03).
func Definitions() *sdkauthz.Definitions {
	none := map[string]sdkauthz.Dimension{}
	return &sdkauthz.Definitions{Component: ID,
		Resources: []sdkauthz.Resource{
			{Type: Tasks, ViewOperation: OpTaskRead, Dimensions: none, Fields: map[string]sdkauthz.Field{}},
			{Type: Projections, ViewOperation: OpAdmin, Dimensions: none, Fields: map[string]sdkauthz.Field{}},
			{Type: System, ViewOperation: OpRecover, Dimensions: none, Fields: map[string]sdkauthz.Field{}},
		},
		Operations: []sdkauthz.Operation{
			{Key: OpTaskRead, ResourceType: Tasks, Purpose: "read", RiskClass: "low"},
			{Key: OpAdmin, ResourceType: Projections, Purpose: "read", RiskClass: "low"},
			{Key: OpRecover, ResourceType: System, Purpose: "read", RiskClass: "low"},
		}}
}
