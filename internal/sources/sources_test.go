package sources

import (
	"testing"

	core "github.com/brickKit/infra-workflow/v3/internal/core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WF08: the configuration's form.
func TestTheConfigurationsForm(t *testing.T) {
	s, err := Parse(" erp.purchase.order.approval=erp/purchase, erp.payment.request.approval=erp/payment|erp/old-pay|infra/flowable ")
	require.NoError(t, err)
	assert.Equal(t, core.Sources{"erp.purchase.order.approval": {Current: "erp/purchase", Previous: []string{}},
		"erp.payment.request.approval": {Current: "erp/payment", Previous: []string{"erp/old-pay", "infra/flowable"}}}, s)
	assert.Equal(t, []string{"erp/old-pay", "erp/payment", "erp/purchase", "infra/flowable"}, Components(s))
	empty, err := Parse("")
	require.NoError(t, err)
	assert.Empty(t, empty)
	for _, bad := range []string{"erp.purchase.order.approval", "erp.purchase.order.approval=", "Erp.x.y=erp/purchase",
		"erp.purchase=erp/purchase", "erp.purchase.order.approval=erp.purchase", "a.b.c=erp/x,a.b.c=erp/y",
		"a.b.c=erp/x||erp/y"} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}
	assert.Equal(t, "erp.purchase", Stream("erp/purchase"))
	assert.Equal(t, "erp/purchase", ComponentOf("erp.purchase"))
}

// WF01: the publisher check.
func TestThePublisherCheck(t *testing.T) {
	s, err := Parse("a.b.approval=x/new|x/old")
	require.NoError(t, err)
	assert.Equal(t, "", Judge(s, "a.b.approval", "x/new", false))
	assert.Equal(t, "", Judge(s, "a.b.approval", "x/old", true), "the previous publisher's own task")
	assert.Equal(t, core.NotChosen, Judge(s, "a.b.approval", "x/old", false), "a new task of the previous publisher")
	assert.Equal(t, core.NotChosen, Judge(s, "a.b.approval", "x/other", true))
	assert.Equal(t, core.NotConfigured, Judge(s, "a.c.approval", "x/new", false))
}
