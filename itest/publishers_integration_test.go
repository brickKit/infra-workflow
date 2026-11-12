//go:build integration

package itest

import (
	"time"

	core "github.com/brickKit/infra-workflow/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/recoverypb"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// publisherCheck: a projection is taken only from the publisher the deployment chose for its task type, or from a
// previous publisher for a task it published before (WF01); one not by the shared task contract is malformed (WF09);
// the refused ones change nothing and are listed for the managers.
func (fx *fixture) publisherCheck() {
	t := fx.t
	forged, unknown, bad, old, oldNew := ids.New().String(), ids.New().String(), ids.New().String(), ids.New().String(),
		ids.New().String()
	// test/pay forges test/buy's task type; test/buy publishes a type nobody configured; a payload off the contract
	fx.send("test/pay", fx.projection("test/pay", forged, "test.buy.order.approval", "pending", 1, []string{sectest.Bob}, ""))
	fx.send("test/buy", fx.projection("test/buy", unknown, "test.buy.invoice.approval", "pending", 1, []string{sectest.Bob}, ""))
	malformed := fx.projection("test/buy", bad, "test.buy.order.approval", "pending", 1, []string{sectest.Bob}, "")
	delete(malformed, "candidates")
	fx.send("test/buy", malformed)
	// the projection names another component than its sender, or its ID is no task ID: malformed too
	other, notID := ids.New().String(), fx.projection("test/buy", "T-1", "test.buy.order.approval", "pending", 1, nil, "")
	fx.send("test/buy", fx.projection("test/pay", other, "test.buy.order.approval", "pending", 1, nil, ""))
	fx.send("test/buy", notID)
	// test/pay on its own stream, writing test/buy as the envelope's source and the projection's component: the
	// stream is test/pay's, so test/pay published it, and it is no projection of test/pay's (WF01, WF09)
	disguised := ids.New().String()
	fx.publishAs("test/pay", "test/buy", "test.pay_task.changed.v1", disguised, 1, events.Full,
		fx.projection("test/buy", disguised, "test.buy.order.approval", "pending", 1, []string{sectest.Bob}, ""))

	// test/old, the previous publisher of the payment approvals: a task it published before goes on; a new one is refused
	_, err := fx.admin.Exec(fx.ctx, "INSERT INTO "+itSchema+".task (component, id, task_type, state, legal_entity_id, data, "+
		"version) VALUES ('test/old', $1, 'test.pay.request.approval', 'pending', $2, '{}', 1)", old, fx.le)
	require.NoError(t, err, "a task test/old published while it was the publisher")
	fx.task("test/old", fx.projection("test/old", old, "test.pay.request.approval", "pending", 2, []string{sectest.Bob}, ""))
	fx.send("test/old", fx.projection("test/old", oldNew, "test.pay.request.approval", "pending", 1, []string{sectest.Bob}, ""))

	want := map[string]string{forged: "not_the_chosen_publisher", unknown: "task_type_not_configured", bad: "malformed",
		oldNew: "not_the_chosen_publisher", other: "malformed", "T-1": "malformed", disguised: "malformed"}
	got := map[string]string{}
	require.Eventually(t, func() bool {
		code, page := fx.alice.do("GET", "/api/workflow/rejected-projections?limit=200", nil)
		if code != 200 {
			return false
		}
		for _, x := range page["projections"].([]any) {
			p := x.(map[string]any)
			got[p["task_id"].(string)] = p["reason"].(string)
		}
		return len(got) >= 7
	}, 10*time.Second, 100*time.Millisecond, "the refused projections are listed")
	for id, reason := range want {
		assert.Equal(t, reason, got[id], id)
	}
	for _, id := range []string{forged, unknown, bad, oldNew, disguised} {
		var n int
		require.NoError(t, fx.admin.QueryRow(fx.ctx, "SELECT count(*) FROM "+itSchema+".task WHERE id = $1", id).Scan(&n))
		assert.Zero(t, n, "a refused projection makes no task: %s", id)
	}
	code, page := fx.bob.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.Equal(t, []string{old}, taskIDs(page), "only the previous publisher's own task")
	code, out := fx.bob.do("GET", "/api/workflow/rejected-projections", nil)
	assert.Contains(t, []int{403, 404}, code, "the managers only: %v", out)
	var audited int
	require.NoError(t, fx.admin.QueryRow(fx.ctx, "SELECT count(*) FROM "+itSchema+".be_audit_log WHERE category = 'security_denial'").
		Scan(&audited))
	assert.GreaterOrEqual(t, audited, 5, "each refusal has its security record")
}

// staleness: a publisher with neither a heartbeat nor a projection for WORKFLOW_STALE_AFTER marks its pending tasks
// possibly not current (WF06); its heartbeat clears it; nothing is closed locally.
func (fx *fixture) staleness() {
	t := fx.t
	id := ids.New().String()
	fx.task("test/pay", fx.projection("test/pay", id, "test.pay.request.approval", "pending", 1, []string{carol}, ""))
	stale := func() any {
		code, one := fx.carol.do("GET", "/api/workflow/tasks/"+id+"?component=test/pay", nil)
		require.Equal(t, 200, code, one)
		return one["stale"]
	}
	assert.Equal(t, false, stale(), "it just arrived")
	require.Eventually(t, func() bool { return stale() == true }, 10*time.Second, 200*time.Millisecond,
		"no heartbeat and no projection for 3 seconds")
	code, page := fx.carol.do("GET", "/api/workflow/tasks", nil)
	require.Equal(t, 200, code, page)
	assert.Contains(t, taskIDs(page), id, "still in the worklist: not closed locally")
	beat := ids.New().String()
	fx.publish("test/pay", "test.pay_task.heartbeat.v1", beat, 1, events.Full, map[string]any{"fact_id": beat,
		"component": "test/pay", "at": time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"), "pending": 1, "paused": 1})
	require.Eventually(t, func() bool { return stale() == false }, 10*time.Second, 200*time.Millisecond, "the heartbeat")
}

// recoveryChecks: a full recovery's checks are the recovery tool's only (be-protocol chapter 14); the worklist audits
// no business object (its tasks are projections a redelivery rebuilds), so the SDK's own checks answer.
func (fx *fixture) recoveryChecks() {
	t := fx.t
	dial := func(as string) *grpc.ClientConn {
		conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, as).ClientConfig(core.ID))))
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	_, err := recoverypb.NewCheckServiceClient(dial("test/buy")).Check(fx.ctx, &recoverypb.CheckRequest{Recovery: "r"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the recovery tool only: %v", err)
	res, err := recoverypb.NewCheckServiceClient(dial("ops/recover")).Check(fx.ctx, &recoverypb.CheckRequest{Recovery: "r"})
	require.NoError(t, err)
	assert.NotEmpty(t, res.GetResults(), "the SDK's own checks run")
}
