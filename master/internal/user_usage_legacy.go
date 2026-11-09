package internal

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

const (
	// legacyClassifyInterval is how often the allocations from before metering are classified.
	legacyClassifyInterval = time.Minute
	// legacyStableTicks is how many classifications in a row a static pool must report the same
	// slot type before its allocations take it.
	legacyStableTicks = 10
)

// legacyClassifier gives the allocations from before metering, whose resources were never
// recorded, the kind of their pool: aux, or nothing on Kubernetes, without slots, and the pool's
// slot type with slots. A kind, once given, never changes.
type legacyClassifier struct {
	rm rm.ResourceManager
	// agentOnly is set when every resource manager is an agent one, so every pool that ever ran
	// here was an agent pool, also the ones the resource manager no longer reports.
	agentOnly bool
	// noAux is set for a dispatcher resource manager, whose pools have no auxiliary containers.
	noAux bool
	// stableTicks is how many steps in a row a static pool must report the same slot type with at
	// least as many agents as it had at any step before.
	stableTicks int
	pools       map[string]*legacyPoolObservation
}

// legacyPoolObservation is what a static pool reported so far.
type legacyPoolObservation struct {
	slotType  devicev1.Type
	ticks     int
	maxAgents int32
}

func newLegacyClassifier(r rm.ResourceManager, rms []*config.ResourceManagerWithPoolsConfig) *legacyClassifier {
	c := &legacyClassifier{
		rm:          r,
		agentOnly:   true,
		stableTicks: legacyStableTicks,
		pools:       map[string]*legacyPoolObservation{},
	}
	for _, cfg := range rms {
		if cfg.ResourceManager.AgentRM == nil {
			c.agentOnly = false
		}
		if cfg.ResourceManager.DispatcherRM != nil || cfg.ResourceManager.PbsRM != nil {
			c.noAux = true
		}
	}
	return c
}

// run classifies until no allocation from before metering is left unclassified.
func (c *legacyClassifier) run(ctx context.Context, interval time.Duration) {
	for {
		done, err := c.step(ctx)
		if err != nil {
			log.WithError(err).Warn("failed to classify allocations from before metering")
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// step classifies what the resource manager's pools tell now; it returns true when nothing is left
// to classify.
func (c *legacyClassifier) step(ctx context.Context) (bool, error) {
	pending, err := db.UnclassifiedAllocationPools(ctx)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		return true, nil
	}
	resp, err := c.rm.GetResourcePools()
	if err != nil {
		return false, err
	}
	reported := map[string]*resourcepoolv1.ResourcePool{}
	for _, p := range resp.GetResourcePools() {
		reported[p.Name] = p
		c.observe(p)
	}
	for _, name := range pending {
		var zeroSlots, withSlots userusage.Kind
		if p, ok := reported[name]; ok {
			noAux := c.noAux || p.Type == resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S
			zeroSlots, _ = userusage.LegacyKind(0, noAux, device.ZeroSlot)
			if slotType, ok := c.slotType(p); ok {
				withSlots, _ = userusage.LegacyKind(1, noAux, slotType)
			}
		} else if c.agentOnly {
			zeroSlots = userusage.KindAux
		}
		n, err := db.ClassifyLegacyAllocations(ctx, name, zeroSlots, withSlots)
		if err != nil {
			return false, err
		}
		if n > 0 {
			log.Infof("classified %d allocations from before metering of resource pool %s", n, name)
		}
	}
	return false, nil
}

// observe records a static pool's slot type and agent count for this step.
func (c *legacyClassifier) observe(p *resourcepoolv1.ResourcePool) {
	if p.Type != resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC {
		return
	}
	o, ok := c.pools[p.Name]
	if !ok {
		o = &legacyPoolObservation{}
		c.pools[p.Name] = o
	}
	o.maxAgents = max(o.maxAgents, p.NumAgents)
	switch {
	case p.SlotType == devicev1.Type_TYPE_UNSPECIFIED || p.NumAgents < o.maxAgents:
		o.slotType, o.ticks = devicev1.Type_TYPE_UNSPECIFIED, 0
	case p.SlotType == o.slotType:
		o.ticks++
	default:
		o.slotType, o.ticks = p.SlotType, 1
	}
}

// slotType returns a pool's slot type once it is known: at once for the pools whose slot type
// comes from the configuration, and for a static pool, which reports the type of the agents
// connected at the moment, once it is stable.
func (c *legacyClassifier) slotType(p *resourcepoolv1.ResourcePool) (device.Type, bool) {
	if p.Type == resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC {
		if o := c.pools[p.Name]; o == nil || o.ticks < c.stableTicks {
			return device.ZeroSlot, false
		}
	}
	switch p.SlotType {
	case devicev1.Type_TYPE_CPU:
		return device.CPU, true
	case devicev1.Type_TYPE_CUDA:
		return device.CUDA, true
	case devicev1.Type_TYPE_ROCM:
		return device.ROCM, true
	default:
		return device.ZeroSlot, false
	}
}
