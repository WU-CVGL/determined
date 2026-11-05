package etc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Static files may sit in subdirectories of the root, but not outside it, even when they exist.
func TestMustStaticFileStaysInRoot(t *testing.T) {
	require.NoError(t, SetRootPath("../../static/srv"))
	require.NotEmpty(t, MustStaticFile(SSHServiceResource))
	require.NotEmpty(t, MustStaticFile(InteractiveSupervisorResource))
	require.NotEmpty(t, MustStaticFile(TensorBoardServiceResource))
	require.NotEmpty(t, MustStaticFile(TensorBoardServiceProgramResource))
	for _, name := range []string{"..", ".", "", "../../../go.mod", "services/../../../../go.mod"} {
		require.Panics(t, func() { MustStaticFile(name) }, name)
	}
}
