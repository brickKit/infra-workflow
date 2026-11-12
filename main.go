// Command infra-workflow runs the worklist alone (serve, migrate); the shells compile in workflow.Definition().
package main

import (
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/infra-workflow/v3/workflow"
)

func main() { launcher.Main(workflow.Definition()) }
