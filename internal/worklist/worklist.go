// Package worklist answers the user surface (docs/design.md WF03-WF07): my pending tasks, what I did, one task with
// its action entry and whether it may not be current. Who sees a task is who its publisher made a candidate (or its
// decider): the worklist judges no other component's authorization, and it keeps no decision.
package worklist

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	core "github.com/brickKit/infra-workflow/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/jackc/pgx/v5"
)

// Task is a worklist entry (the contract's WorklistTask): the projection as it came, where a decision goes (derived
// from the publishing component, never from the projection), whether it may not be current.
type Task struct {
	Task       json.RawMessage `json:"task"`
	Action     Action          `json:"action"`
	Stale      bool            `json:"stale"`
	ReceivedAt string          `json:"received_at"`
	component  string
	id         string
	key        time.Time // the page's ordering value
}

// Action is the action entry: the component and its standard route (be-protocol P17.14).
type Action struct {
	Component string `json:"component"`
	Path      string `json:"path"`
}

// cols reads a task with its staleness (WF06): pending, and its publisher silent (no heartbeat, no projection) for
// longer than the threshold ($1, an interval).
const cols = `t.data, t.component, t.id::text, t.received_at, t.state = 'pending' AND
	coalesce(greatest(p.last_heartbeat, p.last_projection), '-infinity') < now() - $1::interval`

const from = ` FROM task t LEFT JOIN publisher p ON p.component = t.component`

func scan(rows pgx.Rows) (Task, error) {
	var t Task
	var at time.Time
	err := rows.Scan(&t.Task, &t.component, &t.id, &at, &t.Stale)
	t.ReceivedAt = at.UTC().Format(time.RFC3339Nano)
	t.Action = Action{Component: t.component, Path: "/be/flow/tasks/" + t.id + "/decisions"}
	return t, err
}

// page is one page of a query: q selects cols, the ordering key and the pagination condition placeholder %s; args
// begin with the stale interval.
func page(m *core.Base, ctx context.Context, q string, args []any, limit int) ([]Task, error) {
	out := []Task{}
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Task
			var at time.Time
			if err := rows.Scan(&t.Task, &t.component, &t.id, &at, &t.Stale, &t.key); err != nil {
				return err
			}
			t.ReceivedAt = at.UTC().Format(time.RFC3339Nano)
			t.Action = Action{Component: t.component, Path: "/be/flow/tasks/" + t.id + "/decisions"}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// answer writes a page and its cursor (the last row's key, component and ID).
func answer(c *httpx.Context, tasks []Task, limit int) error {
	next := ""
	if len(tasks) > limit {
		tasks = tasks[:limit]
		last := tasks[limit-1]
		next = core.Cursor(last.key.UTC().Format(time.RFC3339Nano), last.component, last.id)
	}
	return c.JSON(200, map[string]any{"tasks": tasks, "next_cursor": next})
}

// query is a list's request: the caller, the page and the filters.
type query struct {
	me    string
	limit int
	after []string
	where []string
	args  []any
}

func request(m *core.Base, c *httpx.Context, filters ...string) (context.Context, query, error) {
	ctx, me, err := core.Check(c, core.OpTaskRead)
	if err != nil {
		return ctx, query{}, err
	}
	q := query{me: me, args: []any{m.StaleAfter.String(), me}}
	if q.limit, err = core.Limit(c.Query("limit")); err != nil {
		return ctx, q, err
	}
	if q.after, err = core.FromCursor(c.Query("cursor"), 3); err != nil {
		return ctx, q, err
	}
	for _, f := range filters {
		v := c.Query(f)
		if v == "" {
			continue
		}
		if f == "legal_entity_id" && !uuidForm(v) {
			return ctx, q, core.Invalid(f, "a legal entity's ID")
		}
		q.args = append(q.args, v)
		q.where = append(q.where, "t."+f+" = $"+strconv.Itoa(len(q.args))+map[bool]string{true: "::uuid", false: ""}[f == "legal_entity_id"])
	}
	return ctx, q, nil
}

// Mine is my pending tasks: I am a current candidate (WF03); the earliest due first, then the oldest received.
func Mine(m *core.Base, c *httpx.Context) error {
	ctx, q, err := request(m, c, "task_type", "legal_entity_id")
	if err != nil {
		return err
	}
	key := `coalesce(t.due_at, t.received_at)`
	where := append([]string{`t.state = 'pending'`, `EXISTS (SELECT 1 FROM task_candidate x WHERE x.component = t.component
		AND x.task_id = t.id AND x.principal = $2 AND x.current)`}, q.where...)
	if q.after != nil {
		q.args = append(q.args, q.after[0], q.after[1], q.after[2])
		n := len(q.args)
		where = append(where, "("+key+", t.component, t.id) > ($"+strconv.Itoa(n-2)+"::timestamptz, $"+strconv.Itoa(n-1)+", $"+strconv.Itoa(n)+"::uuid)")
	}
	q.args = append(q.args, q.limit+1)
	tasks, err := page(m, ctx, `SELECT `+cols+`, `+key+from+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY `+key+`, t.component, t.id LIMIT $`+strconv.Itoa(len(q.args)), q.args, q.limit)
	if err != nil {
		return err
	}
	return answer(c, tasks, q.limit)
}

// Done is what I did: the tasks I decided and the ended tasks I was ever a candidate of (WF04), the latest first.
func Done(m *core.Base, c *httpx.Context) error {
	ctx, q, err := request(m, c, "task_type")
	if err != nil {
		return err
	}
	where := append([]string{`t.state IN ('decided', 'superseded')`, `(t.decided_by = $2 OR EXISTS (SELECT 1
		FROM task_candidate x WHERE x.component = t.component AND x.task_id = t.id AND x.principal = $2))`}, q.where...)
	if q.after != nil {
		q.args = append(q.args, q.after[0], q.after[1], q.after[2])
		n := len(q.args)
		where = append(where, "(t.received_at, t.component, t.id) < ($"+strconv.Itoa(n-2)+"::timestamptz, $"+strconv.Itoa(n-1)+", $"+strconv.Itoa(n)+"::uuid)")
	}
	q.args = append(q.args, q.limit+1)
	tasks, err := page(m, ctx, `SELECT `+cols+`, t.received_at`+from+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY t.received_at DESC, t.component DESC, t.id DESC LIMIT $`+strconv.Itoa(len(q.args)), q.args, q.limit)
	if err != nil {
		return err
	}
	return answer(c, tasks, q.limit)
}

// Get is one task, for who is or was its candidate or decided it (WF05); anyone else gets 404.
func Get(m *core.Base, c *httpx.Context) error {
	ctx, me, err := core.Check(c, core.OpTaskRead)
	if err != nil {
		return err
	}
	component, id := c.Query("component"), c.Param("id")
	if component == "" {
		return core.Invalid("component", "the task's component (a task is its component and its ID)")
	}
	if !uuidForm(id) {
		return core.NotFound("task")
	}
	var out Task
	found := false
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+cols+from+` WHERE t.component = $2 AND t.id = $3::uuid AND (t.decided_by = $4
			OR EXISTS (SELECT 1 FROM task_candidate x WHERE x.component = t.component AND x.task_id = t.id
				AND x.principal = $4))`, m.StaleAfter.String(), component, id, me)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			found = true
			if out, err = scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if !found {
		return core.NotFound("task")
	}
	return c.JSON(200, out)
}

func uuidForm(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdef", r):
			return false
		}
	}
	return true
}
