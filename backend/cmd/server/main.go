package main

import (
	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/v2/backend/module"
)

// 独立运行的入口只有这一行：装配全在 module.New 里，进外壳时外壳调同一个 New。
func main() { besdk.RunStandalone(module.New) }
