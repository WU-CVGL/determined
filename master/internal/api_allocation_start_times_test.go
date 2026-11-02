package internal

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
)

func TestAgentOnlyAllocationStarts(t *testing.T) {
	agent := &config.ResourceManagerConfig{AgentRM: &config.AgentResourceManagerConfig{}}
	kubernetes := &config.ResourceManagerConfig{KubernetesRM: &config.KubernetesResourceManagerConfig{}}
	for _, tc := range []struct {
		name string
		rm   *config.ResourceManagerConfig
		more []*config.ResourceManagerWithPoolsConfig
		want bool
	}{
		{name: "agent", rm: agent, want: true},
		{name: "multiple agent managers", rm: agent, more: []*config.ResourceManagerWithPoolsConfig{
			{ResourceManager: agent},
		}, want: true},
		{name: "kubernetes", rm: kubernetes},
		{name: "slurm", rm: &config.ResourceManagerConfig{DispatcherRM: &config.DispatcherResourceManagerConfig{}}},
		{name: "pbs", rm: &config.ResourceManagerConfig{PbsRM: &config.DispatcherResourceManagerConfig{}}},
		{name: "unknown", rm: &config.ResourceManagerConfig{}},
		{name: "unconfigured"},
		{name: "mixed", rm: agent, more: []*config.ResourceManagerWithPoolsConfig{
			{ResourceManager: kubernetes},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, agentOnlyAllocationStarts(config.ResourceConfig{
				RootManagerInternal: tc.rm, AdditionalResourceManagersInternal: tc.more,
			}))
		})
	}
}
