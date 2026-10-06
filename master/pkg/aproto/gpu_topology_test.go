package aproto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestP2PUsability(t *testing.T) {
	ok := GPUP2PCaps{Read: GPUP2PStatusOK, Write: GPUP2PStatusOK}
	cases := []struct {
		name string
		ab   GPUP2PCaps
		ba   GPUP2PCaps
		want GPUP2PUsability
	}{
		{"all four OK", ok, ok, GPUP2PUsable},
		{
			"READ OK, WRITE NOT_SUPPORTED in one direction", ok,
			GPUP2PCaps{Read: GPUP2PStatusOK, Write: GPUP2PStatusNotSupported},
			GPUP2PNotUsable,
		},
		{
			"READ GPU_NOT_SUPPORTED everywhere (g292)",
			GPUP2PCaps{Read: GPUP2PStatusGPUNotSupported, Write: GPUP2PStatusGPUNotSupported},
			GPUP2PCaps{Read: GPUP2PStatusGPUNotSupported, Write: GPUP2PStatusGPUNotSupported},
			GPUP2PNotUsable,
		},
		{"a failed WRITE query, the rest OK", ok, GPUP2PCaps{Read: GPUP2PStatusOK}, ""},
		{"nothing known", GPUP2PCaps{}, GPUP2PCaps{}, ""},
		{
			"a known non-OK status wins over an unknown one",
			GPUP2PCaps{Read: GPUP2PStatusDisabledByRegkey},
			GPUP2PCaps{},
			GPUP2PNotUsable,
		},
		{
			"an unrecognized string is unknown, not non-OK", ok,
			GPUP2PCaps{Read: GPUP2PStatusOK, Write: "FUTURE_STATUS"},
			"",
		},
		{
			"an unrecognized string and a known non-OK status",
			GPUP2PCaps{Read: "FUTURE_STATUS", Write: GPUP2PStatusOK},
			GPUP2PCaps{Read: GPUP2PStatusChipsetNotSupported, Write: GPUP2PStatusOK},
			GPUP2PNotUsable,
		},
		{
			"TOPOLOGY_NOT_SUPPORTED on WRITE only", ok,
			GPUP2PCaps{Read: GPUP2PStatusOK, Write: GPUP2PStatusTopologyNotSupported},
			GPUP2PNotUsable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, P2PUsability(GPULink{UUIDA: "a", UUIDB: "b", P2PAToB: c.ab, P2PBToA: c.ba}))
			// The rule is symmetric.
			require.Equal(t, c.want, P2PUsability(GPULink{UUIDA: "a", UUIDB: "b", P2PAToB: c.ba, P2PBToA: c.ab}))
		})
	}
}

func TestGPUEnumsKnown(t *testing.T) {
	for _, l := range []GPULinkLevel{"INTERNAL", "PIX", "PXB", "PHB", "NODE", "SYS"} {
		require.True(t, l.Known(), l)
	}
	for _, l := range []GPULinkLevel{"", "NV4", "node"} {
		require.False(t, l.Known(), l)
	}
	for _, s := range []GPUP2PStatus{
		"OK", "CHIPSET_NOT_SUPPORTED", "GPU_NOT_SUPPORTED", "TOPOLOGY_NOT_SUPPORTED",
		"DISABLED_BY_REGKEY", "NOT_SUPPORTED",
	} {
		require.True(t, s.Known(), s)
	}
	for _, s := range []GPUP2PStatus{"", "UNKNOWN", "ok"} {
		require.False(t, s.Known(), s)
	}
}
