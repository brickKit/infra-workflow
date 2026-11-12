//go:build integration

package itest

// The worklist against PostgreSQL and the bus over its real surfaces (L2). The flow managers are the test itself: it
// publishes task projections and heartbeats on the streams of two test components, test/buy and test/pay, as their
// forwarders would (no shared stream of a real component is touched). Needs BE_SDK_TEST_PG_ADMIN and
// BE_SDK_TEST_NATS_URL. The member listens on 28201 (user) and 9201 (system).
// Run: go test -tags integration ./itest/ (make test-integration); the index of the steps is itest/AGENTS.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/brickKit/infra-workflow/v3/internal/core"
	workflow "github.com/brickKit/infra-workflow/v3/workflow"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/component"
	"github.com/brickKit/be-sdk-go/v3/config"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/events/bus"
	"github.com/brickKit/be-sdk-go/v3/events/jetstream"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const (
	itPort   = "28201"
	itBase   = "http://localhost:" + itPort
	itSystem = "localhost:9201"
	itSchema = "infra_workflow_it"
	// the deployment's choice (WF08): test/buy publishes the order and requisition approvals; test/pay the payment
	// approvals, test/old before it (its tasks still come from it)
	sources = "test.buy.order.approval=test/buy,test.buy.requisition.approval=test/buy," +
		"test.pay.request.approval=test/pay|test/old"
	carol = "0190f3a1-0000-7000-8000-0000000000c3" // reads her worklist and nothing else
	dave  = "0190f3a1-0000-7000-8000-0000000000d4" // no grant at all
)

var streams = []events.Stream{{Name: "test.buy", Partitions: 4}, {Name: "test.pay", Partitions: 4}, {Name: "test.old", Partitions: 4}}

// fixture is the running member and the world, shared by the steps.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	w     *sectest.World
	admin *pgx.Conn
	tr    events.Transport
	alice client // manages: the rejected projections
	bob   client
	carol client
	dave  client
	le    string
	tasks map[string]string // what one step leaves for the next, by name
}

func TestWorkflowEndToEnd(t *testing.T) {
	fx := start(t)
	fx.worklist()
	fx.done()
	fx.publisherCheck()
	fx.staleness()
	fx.recoveryChecks()
}

func start(t *testing.T) *fixture {
	adminURL, natsURL := os.Getenv("BE_SDK_TEST_PG_ADMIN"), os.Getenv("BE_SDK_TEST_NATS_URL")
	if adminURL == "" || natsURL == "" {
		t.Fatal("BE_SDK_TEST_PG_ADMIN and BE_SDK_TEST_NATS_URL are needed")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close(ctx) })
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+itSchema+" CASCADE; CREATE SCHEMA "+itSchema)
	require.NoError(t, err)
	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	pw, _ := u.User.Password()
	w := sectest.New(t)
	w.GrantAll(sectest.Alice, core.Definitions())
	read := sdkauthz.Segment{ResourceType: core.Tasks, Operation: core.OpTaskRead, Atoms: map[string]sdkauthz.Atom{},
		Valid: sdkauthz.Window{From: "2026-01-01T00:00:00Z"}}
	w.Grant(sectest.Bob, read)
	w.Grant(carol, read)
	fx := &fixture{t: t, ctx: ctx, w: w, admin: admin, le: ids.New().String(), tasks: map[string]string{}}
	portFree(t, itPort, itSystem)

	// the test streams, clean each run (another run's events would come back)
	js, err := jetstream.Connect(natsURL, "workflow-it-clean")
	require.NoError(t, err)
	for _, s := range streams {
		_ = js.DeleteStream(ctx, s.Name)
	}
	_ = js.Close()
	fx.tr, err = bus.Open(ctx, natsURL, "workflow-it", slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { fx.tr.Close() })
	for _, s := range streams {
		require.NoError(t, fx.tr.EnsureStream(ctx, s))
	}

	env := w.Env(t, core.ID)
	for k, v := range map[string]string{"COMPONENT_ID": core.ID, "COMPONENT_VERSION": workflow.Version, "PORT": itPort,
		"STOP_DRAIN_DELAY": "50ms", "PG_HOST": u.Hostname(), "PG_PORT": u.Port(), "PG_DATABASE": strings.TrimPrefix(u.Path, "/"),
		"PG_USER": u.User.Username(), "PG_PASSWORD": pw, "PG_SCHEMA": itSchema, "PG_OWNER_USER": u.User.Username(),
		"PG_OWNER_PASSWORD": pw, "BUS_URL": natsURL, "IMPLEMENTATION_INSTANCE": ids.New().String(),
		core.KeySources: sources, core.KeyStaleAfter: "3s"} {
		env[k] = v
	}
	var logs bytes.Buffer
	var logMu sync.Mutex
	t.Cleanup(func() {
		if t.Failed() {
			logMu.Lock()
			t.Log(logs.String())
			logMu.Unlock()
		}
	})
	out := &lockedWriter{w: &logs, mu: &logMu}
	opts := launcher.Options{Stdout: out, Stderr: out,
		Env: config.Env{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Read: os.ReadFile}}
	migrate := opts
	migrate.Args = []string{"migrate"}
	def := workflow.Definition()
	newModule := def.New
	def.New = func(rt *runtime.Runtime) (component.Module, error) { return newModule(rt) }
	require.Equal(t, launcher.ExitOK, launcher.Run(ctx, def, migrate))
	sctx, stop := context.WithCancel(ctx)
	done := make(chan int, 1)
	go func() { done <- launcher.Run(sctx, def, opts) }()
	t.Cleanup(func() { stop(); <-done })
	require.Eventually(t, func() bool {
		r, err := http.Get(itBase + "/readyz")
		if err == nil {
			r.Body.Close()
		}
		return err == nil && r.StatusCode == 200
	}, 20*time.Second, 100*time.Millisecond)
	fx.alice = client{t, w.Bearer(sectest.Alice)}
	fx.bob = client{t, w.Bearer(sectest.Bob)}
	fx.carol = client{t, w.Bearer(carol)}
	fx.dave = client{t, w.Bearer(dave)}
	return fx
}

// --- the flow managers' side ---

// projection is a task projection by the shared task contract (be-protocol schemas/flow/task.v1).
func (fx *fixture) projection(from, id, taskType, state string, version int64, candidates []string, decidedBy string) map[string]any {
	if candidates == nil {
		candidates = []string{}
	}
	var decision any
	if decidedBy != "" {
		decision = map[string]any{"decision": "approve", "by": decidedBy, "at": "2026-11-12T10:00:00.000000Z", "comment": nil}
	}
	instance := "running"
	if state == "decided" {
		instance = "approved"
	}
	return map[string]any{"id": id, "task_type": taskType, "component": from,
		"document": map[string]any{"component": from, "type": "doc", "id": ids.New().String(), "number": "D-1"},
		"summary":  map[string]any{"title": "Order D-1", "fields": []any{}}, "candidates": candidates, "state": state,
		"paused_reason": nil, "decision": decision, "due_at": nil,
		"instance":        map[string]any{"id": ids.New().String(), "state": instance, "step": 1},
		"legal_entity_id": fx.le, "version": version, "updated_at": "2026-11-12T10:00:00.000000Z"}
}

// publish puts an event on a test component's stream, as that component's forwarder would (its service identity is
// the source).
func (fx *fixture) publish(from, typ, id string, version int64, kind events.Projection, data map[string]any) {
	fx.t.Helper()
	raw, err := json.Marshal(data)
	require.NoError(fx.t, err)
	stream := events.Stream{Name: strings.ReplaceAll(from, "/", "."), Partitions: 4}
	row := events.Row{EventID: ids.New().String(), Stream: stream.Name, Partition: stream.PartitionOf(id), Type: typ,
		OccurredAt: time.Now(), RecordID: id, Version: version, Projection: kind, Payload: raw, Seq: time.Now().UnixNano(),
		Epoch: events.Epoch{Generation: 1, Timeline: 1}}
	row.Digest = events.Digest(row.EventID, raw)
	require.NoError(fx.t, fx.tr.Publish(fx.ctx, events.Message{Subject: events.Subject(stream.Name, row.Partition, row.Type),
		Headers: row.Headers(from, strings.ReplaceAll(from, "/", "-")+"-it"), Data: raw}))
}

// task publishes a projection on its publisher's stream and waits until the worklist holds that version.
func (fx *fixture) task(from string, p map[string]any) {
	fx.t.Helper()
	fx.send(from, p)
	id, version := p["id"].(string), p["version"].(int64)
	require.Eventually(fx.t, func() bool {
		var v int64
		err := fx.admin.QueryRow(fx.ctx, "SELECT version FROM "+itSchema+".task WHERE component = $1 AND id = $2", from, id).Scan(&v)
		return err == nil && v == version
	}, 10*time.Second, 50*time.Millisecond, "%s's task %s at version %d", from, id, version)
}

// send publishes a projection without waiting.
func (fx *fixture) send(from string, p map[string]any) {
	fx.publish(from, strings.ReplaceAll(from, "/", ".")+"_task.changed.v1", p["id"].(string), p["version"].(int64), events.Full, p)
}

// --- helpers ---

type client struct {
	t     *testing.T
	token string
}

func (c client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, itBase+path, r)
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// ids lists the task IDs of a worklist page.
func taskIDs(page map[string]any) []string {
	out := []string{}
	for _, x := range page["tasks"].([]any) {
		out = append(out, x.(map[string]any)["task"].(map[string]any)["id"].(string))
	}
	return out
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// portFree fails at once when a port the member needs is taken (a running cell or another member would answer).
func portFree(t *testing.T, addrs ...string) {
	for _, a := range addrs {
		if !strings.Contains(a, ":") {
			a = "localhost:" + a
		}
		ln, err := net.Listen("tcp", a)
		require.NoError(t, err, fmt.Sprintf("port %s is taken: is a cell or another member running?", a))
		_ = ln.Close()
	}
}
