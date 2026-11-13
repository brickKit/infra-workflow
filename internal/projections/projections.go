// Package projections takes the task projections and heartbeats of the deployment's chosen publishers (docs/design.md
// WF01, WF02, WF06, WF09): the publisher check, the shape by the shared task contract, the overwrite by version, the
// candidates now and ever, the publishers' last signs of life; the refused projections and their purge.
package projections

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	core "github.com/brickKit/infra-workflow/v3/internal/core"
	sources "github.com/brickKit/infra-workflow/v3/internal/sources"

	protocol "github.com/brickKit/be-protocol/v3"
	"github.com/brickKit/be-sdk-go/v3/audit"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/work"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var uuidForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// contract is the shared task contract (be-protocol schemas/flow/task.v1), compiled once.
var contract = func() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.UseLoader(protocolLoader{})
	return c.MustCompile(protocolBase + "schemas/flow/task.v1.schema.json")
}()

const protocolBase = "https://brickkit.dev/be-protocol/"

// protocolLoader reads the protocol's schemas by their $id from the protocol module.
type protocolLoader struct{}

func (protocolLoader) Load(u string) (any, error) {
	if !strings.HasPrefix(u, protocolBase) {
		return nil, errors.New("no schema at " + u)
	}
	b, err := protocol.Schemas.ReadFile(strings.TrimPrefix(u, protocolBase))
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}

// Subs are the subscriptions the configuration makes (WF08): each named publisher's stream, its task projections and
// heartbeats; the handler is told whose stream it is.
func Subs(m *core.Base, handle func(*core.Base, string, context.Context, db.Tx, events.Received) error) []events.Sub {
	var out []events.Sub
	for _, c := range sources.Components(m.Sources) {
		s, owner := sources.Stream(c), c
		out = append(out, events.Sub{Name: "tasks-" + strings.ReplaceAll(s, ".", "-"), Streams: []string{s},
			Types: []string{s + core.TaskChangedSuffix, s + core.TaskHeartbeatSuffix},
			Handle: func(ctx context.Context, tx db.Tx, e events.Received) error {
				return handle(m, owner, ctx, tx, e)
			}})
	}
	return out
}

// projected is what the worklist reads of a projection to keep it; the rest is kept as it came.
type projected struct {
	ID          string   `json:"id"`
	TaskType    string   `json:"task_type"`
	Component   string   `json:"component"`
	Candidates  []string `json:"candidates"`
	State       string   `json:"state"`
	LegalEntity *string  `json:"legal_entity_id"`
	DueAt       *string  `json:"due_at"`
	Version     int64    `json:"version"`
	Decision    *struct {
		By string `json:"by"`
	} `json:"decision"`
}

// OnEvent takes a task projection or a heartbeat of a subscribed publisher, in the consumer's transaction. owner is the
// component whose stream it came on: the bus lets only its service identity publish there, so it is the publisher;
// the envelope's source and the projection's component are what the publisher wrote, and must both name it (WF01).
func OnEvent(m *core.Base, owner string, ctx context.Context, tx db.Tx, e events.Received) error {
	from := owner
	if strings.HasSuffix(e.Type, core.TaskHeartbeatSuffix) {
		if e.Source != "urn:be:"+owner {
			return nil // a heartbeat that names another component says nothing about this stream's owner
		}
		return seen(ctx, tx, from, "last_heartbeat")
	}
	var p projected
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(e.Data))
	if err != nil || contract.Validate(doc) != nil || json.Unmarshal(e.Data, &p) != nil || p.Component != from ||
		e.Source != "urn:be:"+owner || !uuidForm.MatchString(p.ID) {
		_ = json.Unmarshal(e.Data, &p)
		id := p.ID
		if id == "" {
			id = e.RecordID
		}
		return reject(m, ctx, tx, from, p.TaskType, id, core.Malformed)
	}
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task WHERE component = $1 AND id = $2::uuid)`, from, p.ID).
		Scan(&known); err != nil {
		return err
	}
	if reason := sources.Judge(m.Sources, p.TaskType, from, known); reason != "" {
		return reject(m, ctx, tx, from, p.TaskType, p.ID, reason)
	}
	if err := seen(ctx, tx, from, "last_projection"); err != nil {
		return err
	}
	var decidedBy *string
	if p.Decision != nil {
		decidedBy = &p.Decision.By
	}
	// WF02: a newer version replaces the task whole; an older or the same one changes nothing
	tag, err := tx.Exec(ctx, `INSERT INTO task (component, id, task_type, state, legal_entity_id, due_at, decided_by, data,
			version) VALUES ($1, $2::uuid, $3, $4, $5::uuid, $6::timestamptz, $7, $8, $9)
		ON CONFLICT (component, id) DO UPDATE SET task_type = excluded.task_type, state = excluded.state,
			legal_entity_id = excluded.legal_entity_id, due_at = excluded.due_at, decided_by = excluded.decided_by,
			data = excluded.data, version = excluded.version, received_at = now()
		WHERE task.version < excluded.version`, from, p.ID, p.TaskType, p.State, p.LegalEntity, p.DueAt, decidedBy, e.Data,
		p.Version)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	// the current candidates replaced; who was ever one stays (the done list, WF04)
	if p.Candidates == nil {
		p.Candidates = []string{}
	}
	if _, err := tx.Exec(ctx, `UPDATE task_candidate SET current = false WHERE component = $1 AND task_id = $2::uuid
		AND current AND NOT principal = ANY($3::text[])`, from, p.ID, p.Candidates); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_candidate (component, task_id, principal, current)
		SELECT $1, $2::uuid, unnest($3::text[]), true ON CONFLICT (component, task_id, principal) DO UPDATE SET current = true`,
		from, p.ID, p.Candidates)
	return err
}

// seen records a publisher's last heartbeat or projection (WF06).
func seen(ctx context.Context, tx db.Tx, component, column string) error {
	_, err := tx.Exec(ctx, `INSERT INTO publisher (component, `+column+`) VALUES ($1, now())
		ON CONFLICT (component) DO UPDATE SET `+column+` = now()`, component)
	return err
}

// reject records a refused projection and its security record, changing no task (WF01).
func reject(m *core.Base, ctx context.Context, tx db.Tx, from, taskType, taskID, reason string) error {
	m.RT.Logger.Warn("a task projection was refused", "event", "workflow_projection_refused", "component", from,
		"task_type", taskType, "task", taskID, "reason", reason)
	if _, err := tx.Exec(ctx, `INSERT INTO rejected_projection (id, component, task_type, task_id, reason)
		VALUES ($1, $2, $3, $4, $5)`, ids.New().String(), from, taskType, taskID, reason); err != nil {
		return err
	}
	if m.RT.Audit == nil {
		return nil
	}
	_, err := m.RT.Audit.Write(ctx, tx, audit.Record{Category: audit.SecurityDenial, Operation: core.OpAdmin,
		Outcome: audit.Denied, Actor: from, Reason: reason, Target: audit.TargetOf(core.Projections, taskID, nil),
		Changes: map[string]any{"task_type": taskType}})
	return err
}

// JobPurge is the purge of the refused projections older than RejectedKept.
const JobPurge = "purge-rejected-projections"

// Purge deletes the refused projections past their keeping.
func Purge(m *core.Base, ctx context.Context, _ *work.Run) error {
	return m.RT.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM rejected_projection WHERE at < now() - $1::interval`,
			core.RejectedKept.String())
		return err
	})
}

// Rejected lists the refused projections, the latest first (the managers' troubleshooting, WF01).
func Rejected(m *core.Base, c *httpx.Context) error {
	ctx, _, err := core.Check(c, core.OpAdmin)
	if err != nil {
		return err
	}
	limit, err := core.Limit(c.Query("limit"))
	if err != nil {
		return err
	}
	after, err := core.FromCursor(c.Query("cursor"), 2)
	if err != nil {
		return err
	}
	type row struct {
		Component string `json:"component"`
		TaskType  string `json:"task_type"`
		TaskID    string `json:"task_id"`
		Reason    string `json:"reason"`
		At        string `json:"at"`
		id        string
	}
	out := []row{}
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		q := `SELECT id::text, component, task_type, task_id, reason, at FROM rejected_projection`
		args := []any{limit + 1}
		if after != nil {
			q += ` WHERE (at, id) < ($2::timestamptz, $3::uuid)`
			args = append(args, after[0], after[1])
		}
		rows, err := tx.Query(ctx, q+` ORDER BY at DESC, id DESC LIMIT $1`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			var at time.Time
			if err := rows.Scan(&r.id, &r.Component, &r.TaskType, &r.TaskID, &r.Reason, &at); err != nil {
				return err
			}
			r.At = at.UTC().Format(time.RFC3339Nano)
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = core.Cursor(last.At, last.id)
	}
	return c.JSON(200, map[string]any{"projections": out, "next_cursor": next})
}
