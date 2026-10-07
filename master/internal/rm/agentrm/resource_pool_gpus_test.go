package agentrm

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

func TestPoolGPUPolicy(t *testing.T) {
	for name, c := range map[string]struct {
		scheduler config.SchedulerConfig
		packs     bool
	}{
		"best":             {config.SchedulerConfig{FittingPolicy: best}, true},
		"best, switch on":  {config.SchedulerConfig{FittingPolicy: best, NUMAPacking: ptrs.Ptr(true)}, true},
		"best, switch off": {config.SchedulerConfig{FittingPolicy: best, NUMAPacking: ptrs.Ptr(false)}, false},
		"worst":            {config.SchedulerConfig{FittingPolicy: worst}, false},
	} {
		rp := &resourcePool{config: &config.ResourcePoolConfig{Scheduler: &c.scheduler}}
		policy := rp.newGPUPolicy()
		require.Equal(t, c.packs, policy.packNUMA, name)

		req := &sproto.AllocateRequest{SlotsNeeded: 2}
		sel := policy.selection(req, []*fittingState{{Slots: 2}})
		require.Equal(t, c.packs, sel.packNUMA, name)
		// A multi-agent fit packs too: it takes the free devices the fit counted.
		sel = policy.selection(req, []*fittingState{{Slots: 1}, {Slots: 1}})
		require.Equal(t, c.packs, sel.packNUMA, name)
	}
}
