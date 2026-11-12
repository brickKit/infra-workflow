package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/identity"
)

// Check decides op for the caller (no facts: the worklist's resources have no dimension) and answers the context and
// the caller's subject.
func Check(c *httpx.Context, op string) (context.Context, string, error) {
	s, _ := sdkauthz.From(c.Ctx())
	ctx, _, err := s.Check(c.Ctx(), op, sdkauthz.Facts{})
	if err != nil {
		return ctx, "", err
	}
	u, _ := identity.UserFrom(ctx)
	return ctx, u.Subject, nil
}

// Invalid is a request field that is not valid.
func Invalid(field, why string) error {
	return errs.New(errs.ReasonRequestInvalid, why).WithViolations(errs.Violation{Field: field, Reason: "INVALID", Description: why})
}

// NotFound is what a caller who may not see a record gets too.
func NotFound(what string) error { return errs.New(errs.ReasonNotFound, what+" not found") }

// Cursor is a page's position: the ordering values of its last row, opaque to the caller (protocol 14: lists page by
// cursor only).
func Cursor(parts ...string) string {
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(b)
}

// FromCursor reads a cursor of n parts; "" is the first page (nil).
func FromCursor(s string, n int) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	var parts []string
	if err != nil || json.Unmarshal(b, &parts) != nil || len(parts) != n {
		return nil, Invalid("cursor", "not a cursor of this list")
	}
	return parts, nil
}

// Limit reads a page size: DefaultLimit when absent, 1..MaxLimit.
func Limit(s string) (int, error) {
	if s == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > MaxLimit {
		return 0, Invalid("limit", "limit is 1 to 200")
	}
	return n, nil
}
