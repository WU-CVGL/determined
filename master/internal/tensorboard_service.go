package internal

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// The tensorboard service of a shell: TensorBoard on services.tensorboard.logdir, or on the logs of
// its experiment_ids and trial_ids (see tensorboard_sources.go), from the image, under the path at
// which the master proxies it, tasks.TensorBoardPathPrefixEnvVar, which Command.Start sets from the
// service's record.
const (
	// tensorBoardPortEnvVar is the container port of a shell's TensorBoard.
	tensorBoardPortEnvVar = "DET_TENSORBOARD_PORT"
	// tensorBoardLogDirEnvVar is the directory that a shell's TensorBoard reads, as the config
	// gives it, or tensorBoardSourcesLogDir with sources: the unit resolves a relative one against
	// the task's work directory.
	tensorBoardLogDirEnvVar = "DET_TENSORBOARD_LOGDIR"
	// tensorBoardArgsEnvVar holds the more arguments of a shell's TensorBoard as a JSON array of
	// strings, which the service decodes, so that an argument may hold any character but NUL, which
	// the config refuses.
	tensorBoardArgsEnvVar = "DET_TENSORBOARD_ARGS"
	// tensorBoardTokenEnvVar is the token that TensorBoard, of a shell or a task, requires of every
	// request, which the master's proxy adds to the requests that it passes on.
	tensorBoardTokenEnvVar = "DET_TENSORBOARD_TOKEN" //nolint:gosec // A variable's name.
	// tensorBoardTokenBytes is the entropy of a tensorboard service's token.
	tensorBoardTokenBytes = 32
)

// tensorBoardService is the tensorboard service of a shell: at "<task>:<port>", reached over plain
// HTTP. TensorBoard has no login: the master's proxy authenticates its requests, and Exec limits
// them to the task's owner and admins, as for the other services that run as the owner, since
// TensorBoard reads the owner's files. With sources, the service reads the logs of the task's
// experiments and trials, and the proxy also checks the viewer's permission on them on every
// request. TensorBoard itself refuses requests without the master proxy's token, in every network
// mode. TensorBoard serves under its path prefix, so the proxy keeps the path.
func tensorBoardService(port int, sources bool) tasks.InteractiveService {
	return tasks.InteractiveService{
		Name:         interactiveServiceTensorBoard,
		Port:         port,
		Exec:         true,
		SourcesCheck: sources,
	}
}

// addShellTensorBoard adds the tensorboard service to a shell spec: a port from the services'
// range, outside the spec's other ports, a new token, which the spec keeps in its metadata, as it
// keeps the code-server password, and the port, the log directory, the arguments of the config and
// the token in the container's environment. Its unit and program come with the supervisor's. With
// sources, resolved by resolveTensorBoardSources, TensorBoard reads tensorBoardSourcesLogDir, the
// environment also holds the sources file's path and the storage's environment, and the service's
// files are the sources' (see tensorBoardSourceFiles); otherwise it reads the config's logdir and
// has no files of its own.
func addShellTensorBoard(
	spec *tasks.GenericCommandSpec, sources *tensorBoardSources,
) (archive.Archive, error) {
	service := spec.Config.Services[interactiveServiceTensorBoard]
	if service.LogDir == "" && sources == nil {
		return nil, status.Error(codes.InvalidArgument,
			"services: tensorboard: logdir, experiment_ids or trial_ids is required")
	}
	logDir := service.LogDir
	if sources != nil {
		logDir = tensorBoardSourcesLogDir
	}
	args := service.Args
	if args == nil {
		args = []string{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding the tensorboard arguments: %s", err)
	}
	lo, hi := servicePortRange(interactiveServiceTensorBoard)
	port, err := freeServicePort(spec, interactiveServiceTensorBoard, lo, hi)
	if err != nil {
		return nil, err
	}
	token, err := newTensorBoardToken()
	if err != nil {
		return nil, err
	}
	spec.AddService(tensorBoardService(port, sources != nil))
	spec.Metadata.TensorBoardToken = &token
	addExtraEnvVars(spec, map[string]string{
		tensorBoardPortEnvVar:   strconv.Itoa(port),
		tensorBoardLogDirEnvVar: logDir,
		tensorBoardArgsEnvVar:   string(encoded),
		tensorBoardTokenEnvVar:  token,
	})
	if sources == nil {
		return nil, nil
	}
	addExtraEnvVars(spec, sources.StorageEnv)
	addExtraEnvVars(spec, map[string]string{tensorBoardSourcesEnvVar: tensorBoardSourcesFile})
	return tensorBoardSourceFiles(spec.Base.AgentUserGroup, sources)
}

// newTensorBoardToken returns a new token for a TensorBoard behind the master's proxy.
func newTensorBoardToken() (string, error) {
	secret := make([]byte, tensorBoardTokenBytes)
	if _, err := cryptorand.Read(secret); err != nil {
		return "", status.Errorf(codes.Internal, "generating the tensorboard token: %s", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}
