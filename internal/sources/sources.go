// Package sources is the deployment's choice of publishers (docs/design.md WF01, WF08): the configuration's form,
// the streams and event types it subscribes to, and the publisher check. Pure functions: the projections module
// applies them.
package sources

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	core "github.com/brickKit/infra-workflow/v3/internal/core"
)

var (
	taskTypeForm  = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2,}$`)
	componentForm = regexp.MustCompile(`^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$`)
)

// Parse reads WORKFLOW_TASK_SOURCES: <task type>=<component>[|<previous component>...], comma separated; empty is no
// task type. A malformed entry or a task type given twice is an error (the member does not start).
func Parse(s string) (core.Sources, error) {
	out := core.Sources{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		typ, list, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || !taskTypeForm.MatchString(typ) {
			return nil, fmt.Errorf("%s: %q is not <task type>=<component>[|<previous component>...]", core.KeySources, entry)
		}
		if _, twice := out[typ]; twice {
			return nil, fmt.Errorf("%s: task type %s is given twice", core.KeySources, typ)
		}
		cs := strings.Split(list, "|")
		for _, c := range cs {
			if !componentForm.MatchString(c) {
				return nil, fmt.Errorf("%s: %q of task type %s is not a component ID", core.KeySources, c, typ)
			}
		}
		out[typ] = core.Publishers{Current: cs[0], Previous: cs[1:]}
	}
	return out, nil
}

// Components are every publisher the configuration names, current or previous, once each, sorted.
func Components(s core.Sources) []string {
	var out []string
	for _, p := range s {
		for _, c := range append([]string{p.Current}, p.Previous...) {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Stream is a component's stream (be-protocol P17.15: erp/purchase publishes on erp.purchase).
func Stream(component string) string { return strings.ReplaceAll(component, "/", ".") }

// ComponentOf is the component a stream belongs to (the inverse of Stream).
func ComponentOf(stream string) string { return strings.Replace(stream, ".", "/", 1) }

// Judge is the publisher check of one projection (WF01): "" takes it; otherwise the reason it is refused. from is the
// sending component (the envelope's source, the bus's service identity); known says whether the worklist already has
// this task from that component.
func Judge(s core.Sources, taskType, from string, known bool) string {
	p, ok := s[taskType]
	switch {
	case !ok:
		return core.NotConfigured
	case from == p.Current:
		return ""
	case known && slices.Contains(p.Previous, from):
		return ""
	}
	return core.NotChosen
}
