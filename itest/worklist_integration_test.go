//go:build integration

package itest

import (
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// worklist: my pending tasks are the ones I am a current candidate of (WF03), by version (WF02): a newer version
// replaces the candidates, an older or the same one is dropped; reading one task answers its action entry, derived
// from the publishing component (WF05); filters and pages.
func (fx *fixture) worklist() {
	t := fx.t
	t1, t2, t3 := ids.New().String(), ids.New().String(), ids.New().String()
	fx.tasks["t1"], fx.tasks["t2"], fx.tasks["t3"] = t1, t2, t3
	fx.task("test/buy", fx.projection("test/buy", t1, "test.buy.order.approval", "pending", 1, []string{sectest.Bob}, ""))
	fx.task("test/buy", fx.projection("test/buy", t2, "test.buy.requisition.approval", "pending", 1,
		[]string{sectest.Bob, carol}, ""))
	fx.task("test/pay", fx.projection("test/pay", t3, "test.pay.request.approval", "paused", 1, nil, ""))

	// WF03: Bob's pending tasks; a paused task has no candidates and is in nobody's worklist
	code, page := fx.bob.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.ElementsMatch(t, []string{t1, t2}, taskIDs(page))
	code, page = fx.carol.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.Equal(t, []string{t2}, taskIDs(page))
	code, page = fx.dave.do("GET", "/api/workflow/tasks", nil)
	assert.Contains(t, []int{403, 404}, code, "no grant: %v", page)

	// WF03: filters and pages
	code, page = fx.bob.do("GET", "/api/workflow/tasks?task_type=test.buy.order.approval", nil)
	require.Equal(t, 200, code, page)
	assert.Equal(t, []string{t1}, taskIDs(page))
	code, page = fx.bob.do("GET", "/api/workflow/tasks?legal_entity_id="+ids.New().String(), nil)
	require.Equal(t, 200, code, page)
	assert.Empty(t, taskIDs(page))
	code, first := fx.bob.do("GET", "/api/workflow/tasks?limit=1", nil)
	require.Equal(t, 200, code, first)
	require.Len(t, taskIDs(first), 1)
	require.NotEmpty(t, first["next_cursor"])
	code, second := fx.bob.do("GET", "/api/workflow/tasks?limit=1&cursor="+first["next_cursor"].(string), nil)
	require.Equal(t, 200, code, second)
	require.Len(t, taskIDs(second), 1)
	assert.ElementsMatch(t, []string{t1, t2}, append(taskIDs(first), taskIDs(second)...), "two pages, each task once")
	assert.Equal(t, "", second["next_cursor"], "the last page")
	code, bad := fx.bob.do("GET", "/api/workflow/tasks?cursor=not-a-cursor", nil)
	assert.Equal(t, 400, code, bad)

	// WF05: one task with its action entry; who was never a candidate gets 404
	code, one := fx.bob.do("GET", "/api/workflow/tasks/"+t1+"?component=test/buy", nil)
	require.Equal(t, 200, code, one)
	assert.Equal(t, map[string]any{"component": "test/buy", "path": "/api/buy/flow/tasks/" + t1 + "/decisions"}, one["action"])
	assert.Equal(t, false, one["stale"])
	assert.Equal(t, "test.buy.order.approval", at(one, "task", "task_type"))
	code, out := fx.carol.do("GET", "/api/workflow/tasks/"+t1+"?component=test/buy", nil)
	assert.Equal(t, 404, code, out)
	code, out = fx.bob.do("GET", "/api/workflow/tasks/"+t1+"?component=test/pay", nil)
	assert.Equal(t, 404, code, "a task is its component and its ID: %v", out)
	code, out = fx.bob.do("GET", "/api/workflow/tasks/"+t1, nil)
	assert.Equal(t, 400, code, "the component is required: %v", out)

	// WF02: version 2 moves t1 to Carol; version 1 again is dropped
	fx.task("test/buy", fx.projection("test/buy", t1, "test.buy.order.approval", "pending", 2, []string{carol}, ""))
	fx.send("test/buy", fx.projection("test/buy", t1, "test.buy.order.approval", "pending", 1, []string{sectest.Bob}, ""))
	fx.task("test/buy", fx.projection("test/buy", ids.New().String(), "test.buy.order.approval", "pending", 1, nil, "")) // a marker after it
	code, page = fx.bob.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.Equal(t, []string{t2}, taskIDs(page), "t1 left Bob's worklist and the older version did not bring it back")
	code, page = fx.carol.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.ElementsMatch(t, []string{t1, t2}, taskIDs(page))
}

// done: what I decided, and the ended tasks I was a candidate of, the latest first (WF04); an ever-candidate still
// reads the task (WF05).
func (fx *fixture) done() {
	t := fx.t
	t1, t2 := fx.tasks["t1"], fx.tasks["t2"]
	fx.task("test/buy", fx.projection("test/buy", t1, "test.buy.order.approval", "decided", 3, nil, carol))
	fx.task("test/buy", fx.projection("test/buy", t2, "test.buy.requisition.approval", "superseded", 2, nil, ""))
	code, page := fx.carol.do("GET", "/api/workflow/tasks/done", nil)
	require.Equal(t, 200, code, page)
	assert.Equal(t, []string{t2, t1}, taskIDs(page), "the latest first")
	code, page = fx.bob.do("GET", "/api/workflow/tasks/done", nil)
	require.Equal(t, 200, code, page)
	assert.ElementsMatch(t, []string{t1, t2}, taskIDs(page), "Bob was a candidate of both")
	code, page = fx.bob.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.Empty(t, taskIDs(page))
	code, one := fx.bob.do("GET", "/api/workflow/tasks/"+t1+"?component=test/buy", nil)
	require.Equal(t, 200, code, one)
	assert.Equal(t, "decided", at(one, "task", "state"))
	assert.Equal(t, carol, at(one, "task", "decision", "by"))
	// WF03: a task that left pending is in no worklist, whoever its projection still names
	ended := ids.New().String()
	fx.task("test/buy", fx.projection("test/buy", ended, "test.buy.order.approval", "superseded", 1, []string{sectest.Bob}, ""))
	code, page = fx.bob.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.NotContains(t, taskIDs(page), ended)
	code, page = fx.bob.do("GET", "/api/workflow/tasks/done?task_type=test.buy.order.approval", nil)
	require.Equal(t, 200, code, page)
	assert.Equal(t, []string{ended, t1}, taskIDs(page))
}

// at reads a value along a path of keys from a decoded JSON object.
func at(v any, path ...string) any {
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}
