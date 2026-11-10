package userusage

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func summary(typ sproto.ResourcesType, agents ...[]device.Type) sproto.ResourcesSummary {
	s := sproto.ResourcesSummary{ResourcesType: typ, AgentDevices: map[aproto.ID][]device.Device{}}
	for i, types := range agents {
		var ds []device.Device
		for j, t := range types {
			ds = append(ds, device.Device{ID: device.ID(j), Type: t})
		}
		s.AgentDevices[aproto.ID(rune('a'+i))] = ds
	}
	return s
}

func TestAssigned(t *testing.T) {
	docker, k8s, slurm := sproto.ResourcesTypeDockerContainer, sproto.ResourcesTypeK8sJob,
		sproto.ResourcesTypeSlurmJob
	cuda, rocm, cpu := device.CUDA, device.ROCM, device.CPU
	for _, tc := range []struct {
		name     string
		rs       []sproto.ResourcesSummary
		slots    int
		kind     Kind
		quantity int
	}{
		{"cuda", []sproto.ResourcesSummary{summary(docker, []device.Type{cuda, cuda})}, 2, KindGPU, 2},
		{"rocm", []sproto.ResourcesSummary{summary(docker, []device.Type{rocm})}, 1, KindGPU, 1},
		{"cpu", []sproto.ResourcesSummary{summary(docker, []device.Type{cpu, cpu, cpu})}, 3, KindCPU, 3},
		{"no devices", []sproto.ResourcesSummary{summary(docker, nil)}, 0, KindAux, 1},
		{"no agent", []sproto.ResourcesSummary{summary(docker)}, 0, KindAux, 1},
		{
			"gpu and cpu, only the gpus count",
			[]sproto.ResourcesSummary{summary(docker, []device.Type{cuda, cpu, cpu})},
			1, KindGPU, 1,
		},
		{
			"containers on two agents",
			[]sproto.ResourcesSummary{
				summary(docker, []device.Type{cuda, cuda}),
				summary(docker, []device.Type{cuda, cuda}, []device.Type{cuda}),
			},
			5, KindGPU, 5,
		},
		{"unknown device type", []sproto.ResourcesSummary{summary(docker, []device.Type{"tpu"})}, 1, KindUnclassified, 1},
		{"k8s cuda", []sproto.ResourcesSummary{summary(k8s, []device.Type{cuda, cuda})}, 2, KindGPU, 2},
		{"k8s cpu", []sproto.ResourcesSummary{summary(k8s, []device.Type{cpu})}, 1, KindCPU, 1},
		{"k8s no devices", []sproto.ResourcesSummary{summary(k8s, nil)}, 0, KindNone, 0},
		{"slurm", []sproto.ResourcesSummary{summary(slurm)}, 8, KindUnclassified, 8},
		{"slurm no slots", []sproto.ResourcesSummary{summary(slurm)}, 0, KindNone, 0},
		{"nothing", nil, 1, KindNone, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, quantity := Assigned(tc.rs, tc.slots)
			require.Equal(t, tc.kind, kind)
			require.Equal(t, tc.quantity, quantity)
		})
	}
}

func TestLegacyKind(t *testing.T) {
	for _, tc := range []struct {
		slots    int
		k8s      bool
		slotType device.Type
		kind     Kind
		ok       bool
	}{
		{0, false, device.ZeroSlot, KindAux, true},
		{0, false, device.CUDA, KindAux, true},
		{0, true, device.CUDA, KindNone, true},
		{2, false, device.CUDA, KindGPU, true},
		{2, false, device.ROCM, KindGPU, true},
		{2, true, device.CUDA, KindGPU, true},
		{2, false, device.CPU, KindCPU, true},
		{2, true, device.CPU, KindCPU, true},
		// An unspecified slot type is not GPU.
		{2, false, device.ZeroSlot, "", false},
		{2, true, device.ZeroSlot, "", false},
	} {
		kind, ok := LegacyKind(tc.slots, tc.k8s, tc.slotType)
		require.Equal(t, tc.kind, kind, "%+v", tc)
		require.Equal(t, tc.ok, ok, "%+v", tc)
	}
}
