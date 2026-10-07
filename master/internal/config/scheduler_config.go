package config

import (
	"encoding/json"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/union"
)

const (
	// DefaultSchedulingPriority is the default resource manager priority.
	DefaultSchedulingPriority = 42

	// FairShareScheduling schedules tasks proportional to the available resources.
	FairShareScheduling = "fair_share"
	// PriorityScheduling schedules tasks based on their priority.
	PriorityScheduling = "priority"
	// RoundRobinScheduling schedules tasks based on the order in which they arrive.
	RoundRobinScheduling = "round_robin"

	best             = "best"
	worst            = "worst"
	defaultFitPolicy = best
)

// DefaultSchedulerConfig returns the default fair share configuration for the scheduler.
func DefaultSchedulerConfig() *SchedulerConfig {
	tmp := DefaultSchedulingPriority
	return &SchedulerConfig{
		Priority: &PrioritySchedulerConfig{
			Preemption:      false,
			DefaultPriority: &tmp,
		},
		FittingPolicy: defaultFitPolicy,
	}
}

// SchedulerConfig holds the configurations for scheduling policies.
type SchedulerConfig struct {
	FairShare              *FairShareSchedulerConfig  `union:"type,fair_share" json:"-"`
	Priority               *PrioritySchedulerConfig   `union:"type,priority" json:"-"`
	RoundRobin             *RoundRobinSchedulerConfig `union:"type,round_robin" json:"-"`
	FittingPolicy          string                     `json:"fitting_policy"`
	AllowHeterogeneousFits bool                       `json:"allow_heterogeneous_fits"`
	// NUMAPacking turns off NUMA packing when false: under fitting_policy best, the agent RM
	// chooses each task's GPUs inside the agent by NUMA node. Unset means on. It never changes
	// how agents are chosen, and it has no effect under worst.
	NUMAPacking *bool `json:"numa_packing,omitempty"`
}

// PacksGPUsByNUMA reports whether the pool chooses each task's GPUs inside the agent by NUMA node:
// fitting_policy best, and numa_packing unset or true.
func (s SchedulerConfig) PacksGPUsByNUMA() bool {
	return s.FittingPolicy == best && (s.NUMAPacking == nil || *s.NUMAPacking)
}

// MarshalJSON implements the json.Marshaler interface.
func (s SchedulerConfig) MarshalJSON() ([]byte, error) {
	return union.Marshal(s)
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (s *SchedulerConfig) UnmarshalJSON(data []byte) error {
	if err := union.Unmarshal(data, s); err != nil {
		return err
	}

	type DefaultParser *SchedulerConfig
	if err := json.Unmarshal(data, DefaultParser(s)); err != nil {
		return err
	}

	// Fill in the default
	if s.FairShare == nil && s.Priority == nil && s.RoundRobin == nil {
		s.Priority = &PrioritySchedulerConfig{}
	}
	if s.Priority != nil && s.Priority.DefaultPriority == nil {
		defaultPriority := DefaultSchedulingPriority
		s.Priority.DefaultPriority = &defaultPriority
	}
	if s.FittingPolicy == "" {
		s.FittingPolicy = best
	}

	return nil
}

// Validate implements the check.Validatable interface.
func (s SchedulerConfig) Validate() []error {
	return []error{
		check.Contains(
			s.FittingPolicy, []interface{}{best, worst}, "invalid fitting policy",
		),
	}
}

// GetType returns the type of scheduler that is configured.
func (s *SchedulerConfig) GetType() string {
	switch {
	case s.FairShare != nil:
		return FairShareScheduling
	case s.Priority != nil:
		return PriorityScheduling
	case s.RoundRobin != nil:
		return RoundRobinScheduling
	default:
		panic("neither scheduler type configured")
	}
}

// GetPreemption returns whether the scheduler is set to preempt.
func (s *SchedulerConfig) GetPreemption() bool {
	var preemptionEnabled bool
	switch {
	case s.FairShare != nil:
		preemptionEnabled = true
	case s.Priority != nil:
		preemptionEnabled = s.Priority.Preemption
	case s.RoundRobin != nil:
		preemptionEnabled = false
	}
	return preemptionEnabled
}

// FairShareSchedulerConfig holds configurations for the fair share scheduler.
type FairShareSchedulerConfig struct{}

// PrioritySchedulerConfig holds the configurations for the priority scheduler.
type PrioritySchedulerConfig struct {
	Preemption      bool `json:"preemption"`
	DefaultPriority *int `json:"default_priority"`
}

// RoundRobinSchedulerConfig holds the configurations for the round robing scheduler.
type RoundRobinSchedulerConfig struct{}

// Validate implements the check.Validatable interface.
func (p PrioritySchedulerConfig) Validate() []error {
	return model.ValidatePrioritySetting(p.DefaultPriority)
}
