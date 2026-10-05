package detect

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/pkg/device"
)

// detectors are the device detection functions; tests replace them.
type detectors struct {
	cuda func(visibleGPUs string) ([]device.Device, error)
	rocm func(visibleGPUs string) ([]device.Device, error)
	cpu  func() ([]device.Device, error)
}

var defaultDetectors = detectors{cuda: detectCudaGPUs, rocm: detectRocmGPUs, cpu: detectCPUs}

// Detect the devices available. If artificial devices are configured, prefers those, otherwise,
// we detect cuda, rocm, cpu (or no) devices based on the configured slot type.
//
// exclude is the agent's GPU exclude list (exclude_gpus): CUDA GPUs whose UUID is listed are
// returned in excluded instead of devices, keep their nvidia-smi index as device ID, and are never
// offered as slots. With slot type auto they count as found CUDA GPUs, so an agent whose GPUs are
// all excluded has no slots rather than ROCm or CPU slots. An entry that matches no detected CUDA
// GPU is an error, so that a typo never hands an excluded GPU to tasks.
func Detect(
	slotType, agentID, visibleGPUs string, exclude []string, artificialSlots int,
) (devices, excluded []device.Device, err error) {
	// Log detected nvidia version.
	v, err := getNvidiaVersion()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get nvidia version: %w", err)
	} else if v != "" {
		log.Infof("Nvidia driver version: %s", v)
	}

	// Log detected rocm version.
	v, err = getRocmVersion()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get rocm version: %w", err)
	} else if v != "" {
		log.Infof("Rocm driver version: %s", v)
	}

	return detectWith(defaultDetectors, slotType, agentID, visibleGPUs, exclude, artificialSlots)
}

func detectWith(
	d detectors, slotType, agentID, visibleGPUs string, exclude []string, artificialSlots int,
) (devices, excluded []device.Device, err error) {
	detected, err := detectDevices(d, slotType, agentID, visibleGPUs, artificialSlots)
	if err != nil {
		return nil, nil, err
	}
	devices, excluded, err = SplitExcluded(detected, exclude)
	if err != nil {
		return nil, nil, err
	}

	log.Info("detected compute devices:")
	for _, dev := range devices {
		log.Infof("\t%s", dev.String())
	}
	for _, dev := range excluded {
		log.Infof("\t%s %s: excluded by exclude_gpus, not a slot", dev.String(), dev.UUID)
	}
	return devices, excluded, nil
}

// ParseExcludeGPUs splits the exclude_gpus option, a comma-separated list of GPU UUIDs.
func ParseExcludeGPUs(s string) []string {
	var uuids []string
	seen := map[string]bool{}
	for _, u := range strings.Split(s, ",") {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			uuids = append(uuids, u)
		}
	}
	return uuids
}

// SplitExcluded moves every CUDA device whose UUID is in exclude from detected to excluded. The
// others keep their order and device IDs. It fails, naming them, when entries match no CUDA
// device.
func SplitExcluded(
	detected []device.Device, exclude []string,
) (devices, excluded []device.Device, err error) {
	if len(exclude) == 0 {
		return detected, nil, nil
	}
	listed := map[string]bool{}
	for _, u := range exclude {
		listed[u] = true
	}
	matched := map[string]bool{}
	for _, dev := range detected {
		if dev.Type == device.CUDA && listed[dev.UUID] {
			matched[dev.UUID] = true
			excluded = append(excluded, dev)
		} else {
			devices = append(devices, dev)
		}
	}

	var unmatched []string
	indices := false
	for _, u := range exclude {
		if !matched[u] {
			unmatched = append(unmatched, strconv.Quote(u))
			if _, err := strconv.Atoi(u); err == nil {
				indices = true
			}
		}
	}
	if len(unmatched) > 0 {
		msg := fmt.Sprintf("exclude_gpus: no detected CUDA GPU has the UUID %s",
			strings.Join(unmatched, ", "))
		if indices {
			msg += " (exclude_gpus takes GPU UUIDs, not indices)"
		}
		return nil, nil, errors.New(msg)
	}
	if devices == nil {
		devices = []device.Device{}
	}
	return devices, excluded, nil
}

// detectDevices runs today's detection, before the exclude list.
func detectDevices(
	d detectors, slotType, agentID, visibleGPUs string, artificialSlots int,
) ([]device.Device, error) {
	var err error
	var detected []device.Device
	switch {
	case artificialSlots > 0:
		// Generate random UUIDs consistent across agent restarts as long as
		// agentID is the same.
		rnd, sErr := randFromString(agentID)
		if sErr != nil {
			return nil, sErr
		}

		for i := 0; i < artificialSlots; i++ {
			u, rErr := uuid.NewRandomFromReader(rnd)
			if rErr != nil {
				return nil, rErr
			}
			id := u.String()
			detected = append(detected, device.Device{
				ID: device.ID(i), Brand: "Artificial", UUID: id, Type: device.CPU,
			})
		}
	case slotType == "none":
		detected = []device.Device{}
	case slotType == "cuda" || slotType == "gpu":
		// Support "gpu" for backwards compatibility.
		detected, err = d.cuda(visibleGPUs)
		if err != nil {
			return nil, errors.Wrap(
				err,
				"error while gathering GPU info through nvidia-smi command",
			)
		}
	case slotType == "rocm":
		detected, err = d.rocm(visibleGPUs)
		if err != nil {
			return nil, errors.Wrap(err, "error while gathering GPU info through rocm-smi command")
		}
	case slotType == "cpu":
		detected, err = d.cpu()
		if err != nil {
			return nil, err
		}
	case slotType == "auto":
		detected, err = d.cuda(visibleGPUs)
		if err != nil {
			return nil, errors.Wrap(
				err,
				"error while gathering GPU info through nvidia-smi command",
			)
		}
		if len(detected) == 0 {
			detected, err = d.rocm(visibleGPUs)
			if err != nil {
				return nil, errors.Wrap(
					err,
					"error while gathering GPU info through rocm-smi command",
				)
			}
		}
		if len(detected) == 0 {
			detected, err = d.cpu()
			if err != nil {
				return nil, err
			}
		}
	default:
		panic("unrecognized slot type")
	}

	return detected, nil
}

// randFromString returns a random-number generated seeded from an input string.
func randFromString(seed string) (*rand.Rand, error) {
	h := sha256.New()
	h.Write([]byte(seed))
	rndSource, bytesRead := binary.Varint(h.Sum(nil)[:8])
	if bytesRead <= 0 {
		return nil, fmt.Errorf(
			"failed to init random source for artificial slots ids. bytes read: %d", bytesRead)
	}
	rnd := rand.New(rand.NewSource(rndSource)) // nolint:gosec
	return rnd, nil
}
