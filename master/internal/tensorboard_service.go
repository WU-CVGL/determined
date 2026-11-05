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

// The tensorboard service of a shell: TensorBoard on services.tensorboard.logdir, from the image,
// under the path at which the master proxies it, tasks.TensorBoardPathPrefixEnvVar, which
// Command.Start sets from the service's record.
const (
	// tensorBoardPortEnvVar is the container port of a shell's TensorBoard.
	tensorBoardPortEnvVar = "DET_TENSORBOARD_PORT"
	// tensorBoardLogDirEnvVar is the directory that a shell's TensorBoard reads, as the config
	// gives it: the unit resolves a relative one against the task's work directory.
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
// TensorBoard reads the owner's files. TensorBoard itself refuses requests without the master
// proxy's token, in every network mode. TensorBoard serves under its path prefix, so the proxy
// keeps the path.
func tensorBoardService(port int) tasks.InteractiveService {
	return tasks.InteractiveService{
		Name: interactiveServiceTensorBoard,
		Port: port,
		Exec: true,
	}
}

// addShellTensorBoard adds the tensorboard service to a shell spec: a port from the services'
// range, outside the spec's other ports, a new token, which the spec keeps in its metadata, as it
// keeps the code-server password, and the port, the log directory, the arguments of the config and
// the token in the container's environment. The service has no files of its own: its unit and
// program come with the supervisor's.
func addShellTensorBoard(spec *tasks.GenericCommandSpec) (archive.Archive, error) {
	service := spec.Config.Services[interactiveServiceTensorBoard]
	if service.LogDir == "" {
		return nil, status.Error(codes.InvalidArgument, "services: tensorboard: logdir is required")
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
	spec.AddService(tensorBoardService(port))
	spec.Metadata.TensorBoardToken = &token
	addExtraEnvVars(spec, map[string]string{
		tensorBoardPortEnvVar:   strconv.Itoa(port),
		tensorBoardLogDirEnvVar: service.LogDir,
		tensorBoardArgsEnvVar:   string(encoded),
		tensorBoardTokenEnvVar:  token,
	})
	return nil, nil
}

// newTensorBoardToken returns a new token for a TensorBoard behind the master's proxy.
func newTensorBoardToken() (string, error) {
	secret := make([]byte, tensorBoardTokenBytes)
	if _, err := cryptorand.Read(secret); err != nil {
		return "", status.Errorf(codes.Internal, "generating the tensorboard token: %s", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}
