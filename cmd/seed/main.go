// Command seed fills a running infra/workflow with the seed data for people (be-assembly-standard's seed and test data
// conventions). The worklist owns no data of its own to seed: its tasks are the projections the flow managers publish
// (erp/purchase's and erp/payment's seeds leave submitted documents waiting). It grants, by the authorization admin:
//
//   - the role maintain to the seed user: the worklist and the refused projections;
//   - the role worklist to the approvers of the seeds, test.buyer.lead and test.finance.lead, and to the buyers and
//     cashier who submit and follow: their own worklists (what they see is what the publishers made them candidates of).
//
// A second run changes nothing. It waits until the grants reach the component (each account's worklist answers).
//
//	SEED_IAM_URL (http://localhost:18200)    the identity member's development exchange
//	SEED_AUTHZ_URL (http://localhost:18223)  the authorization member's effect point
//	SEED_ADMIN (test.alice)                  the authorization admin (the install claim)
//	SEED_USER (test.bob), SEED_PASSWORD      the seed user; the test accounts' password
//	INFRA_WORKFLOW_URL (http://localhost:18201)
package main

import (
	"fmt"
	"os"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/seed"
)

const prefix = "「本地测试」"

// users are the accounts that get a worklist: the approvers and the ones who submit and follow.
var users = []string{"test.buyer.lead", "test.finance.lead", "test.buyer.cn", "test.cashier.cn"}

func main() {
	base := env("INFRA_WORKFLOW_URL", "http://localhost:18201")
	if !seed.Ready(base) {
		fail("infra/workflow at %s is not ready", base)
	}
	pw := os.Getenv("SEED_PASSWORD")
	if pw == "" {
		fail("SEED_PASSWORD is not set (the test accounts' password in Casdoor)")
	}
	iam, user, admin := env("SEED_IAM_URL", "http://localhost:18200"), env("SEED_USER", "test.bob"), env("SEED_ADMIN", "test.alice")
	accounts, err := seed.Accounts(iam, pw, append([]string{admin, user}, users...)...)
	check(err)
	a, err := seed.NewAuthz(env("SEED_AUTHZ_URL", "http://localhost:18223"), accounts[admin].Token, "infra/workflow")
	check(err)
	read := segment("infra.workflow.task", "infra.workflow.task.read")
	_, err = a.GrantRoles([]seed.RoleSpec{
		{Key: "maintain", Name: prefix + "待办维护", To: []string{user},
			Segments: []sdkauthz.Segment{read, segment("infra.workflow.projection", "infra.workflow.projection.read")}},
		{Key: "worklist", Name: prefix + "我的待办", To: users, Segments: []sdkauthz.Segment{read}},
	}, accounts)
	check(err)
	// the worklist has no write to probe: without its grant the list answers 404 (the operation is checked first), so a
	// 200 is the grant arrived
	for _, u := range append([]string{user}, users...) {
		code, out, err := seed.NewClient(base, accounts[u].Token).UntilGranted(time.Minute, "GET", "/api/workflow/tasks", nil)
		check(err)
		if code != 200 {
			fail("%s's worklist: %d %v", u, code, out)
		}
	}
	fmt.Printf("seed infra/workflow: the worklist granted to %d accounts\n", len(users)+1)
}

// segment grants an operation on a resource of the worklist (no dimension: the candidates bound what one sees).
func segment(resource, op string) sdkauthz.Segment {
	s := seed.Segment(resource, op)
	s.Atoms = map[string]sdkauthz.Atom{}
	return s
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "seed infra/workflow: "+format+"\n", a...)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}
