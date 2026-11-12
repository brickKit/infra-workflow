package workflow

import "github.com/brickKit/be-sdk-go/v3/recovery"

// RecoveryChecks registers what a full recovery asks this member (be-protocol chapter 14): nothing of its own. Its
// tasks are projections of their publishers, rebuilt by redelivery (the SDK's own checks cover the consumer's
// progress); it audits no business object and numbers nothing.
func (m *Module) RecoveryChecks(c *recovery.Checks) error { return nil }
