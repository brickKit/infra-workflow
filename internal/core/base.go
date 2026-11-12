package core

import (
	"time"

	"github.com/brickKit/be-sdk-go/v3/runtime"
)

// Base is what every module works with: the component's runtime, the deployment's chosen publishers and the stale
// threshold.
type Base struct {
	RT         *runtime.Runtime
	Sources    Sources
	StaleAfter time.Duration
}

// Sources are the deployment's chosen publishers (WF08): for each task type its publisher and its previous ones.
type Sources map[string]Publishers

// Publishers is a task type's publisher and the ones it replaced.
type Publishers struct {
	Current  string
	Previous []string
}
