package detect

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/device"
)

// fakeDetectors detects n CUDA GPUs with nvidia-smi indexes as IDs, and fails the test if the
// ROCm or CPU fallback runs.
func fakeDetectors(t *testing.T, n int) detectors {
	return detectors{
		cuda: func(string) ([]device.Device, error) {
			devices := []device.Device{}
			for i := 0; i < n; i++ {
				devices = append(devices, device.Device{
					ID: device.ID(i), Brand: "NVIDIA GeForce RTX 4090", UUID: fmt.Sprintf("GPU-%d", i),
					Type: device.CUDA,
				})
			}
			return devices, nil
		},
		rocm: func(string) ([]device.Device, error) {
			require.Fail(t, "no ROCm fallback")
			return nil, nil
		},
		cpu: func() ([]device.Device, error) {
			require.Fail(t, "no CPU fallback")
			return nil, nil
		},
	}
}

func ids(devices []device.Device) []int {
	out := []int{}
	for _, d := range devices {
		out = append(out, int(d.ID))
	}
	return out
}

func TestDetectExcludeGPUs(t *testing.T) {
	d := fakeDetectors(t, 8)

	// One excluded UUID: the others keep their nvidia-smi index as device ID.
	devices, excluded, err := detectWith(d, "auto", "agent", "", []string{"GPU-4"}, 0)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3, 5, 6, 7}, ids(devices))
	require.Equal(t, []device.Device{{
		ID: 4, Brand: "NVIDIA GeForce RTX 4090", UUID: "GPU-4", Type: device.CUDA,
	}}, excluded)

	// An empty list gives today's result.
	devices, excluded, err = detectWith(d, "cuda", "agent", "", nil, 0)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, ids(devices))
	require.Empty(t, excluded)

	// An entry that matches no detected GPU stops agent start, naming it.
	_, _, err = detectWith(d, "auto", "agent", "", []string{"GPU-4", "GPU-typo"}, 0)
	require.EqualError(t, err, `exclude_gpus: no detected CUDA GPU has the UUID "GPU-typo"`)
	_, _, err = detectWith(d, "auto", "agent", "", []string{"4"}, 0)
	require.EqualError(t, err, `exclude_gpus: no detected CUDA GPU has the UUID "4" `+
		`(exclude_gpus takes GPU UUIDs, not indices)`)

	// All GPUs excluded with slot type auto: no slots, and no ROCm or CPU slots.
	var all []string
	for i := 0; i < 8; i++ {
		all = append(all, fmt.Sprintf("GPU-%d", i))
	}
	devices, excluded, err = detectWith(d, "auto", "agent", "", all, 0)
	require.NoError(t, err)
	require.NotNil(t, devices)
	require.Empty(t, devices)
	require.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, ids(excluded))

	// Only CUDA GPUs can be excluded: with CPU slots every entry is unmatched.
	cpuOnly := detectors{cpu: func() ([]device.Device, error) {
		return []device.Device{{ID: 0, Brand: "cpu", UUID: "GPU-0", Type: device.CPU}}, nil
	}}
	_, _, err = detectWith(cpuOnly, "cpu", "agent", "", []string{"GPU-0"}, 0)
	require.EqualError(t, err, `exclude_gpus: no detected CUDA GPU has the UUID "GPU-0"`)
}

func TestParseExcludeGPUs(t *testing.T) {
	require.Nil(t, ParseExcludeGPUs(""))
	require.Nil(t, ParseExcludeGPUs(" , "))
	require.Equal(t, []string{"GPU-a", "GPU-b"}, ParseExcludeGPUs(" GPU-a,GPU-b ,GPU-a,"))
}

func TestSplitExcludedKeepsOrder(t *testing.T) {
	detected := []device.Device{
		{ID: 0, UUID: "GPU-0", Type: device.CUDA},
		{ID: 1, UUID: "GPU-1", Type: device.CUDA},
		{ID: 2, UUID: "GPU-2", Type: device.CUDA},
	}
	devices, excluded, err := SplitExcluded(detected, []string{"GPU-2", "GPU-0"})
	require.NoError(t, err)
	require.Equal(t, []int{1}, ids(devices))
	require.Equal(t, []int{0, 2}, ids(excluded), "excluded GPUs keep detection order")

	devices, excluded, err = SplitExcluded(detected, nil)
	require.NoError(t, err)
	require.Equal(t, detected, devices)
	require.Nil(t, excluded)
}
