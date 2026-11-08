package db

import (
	"fmt"

	"github.com/o1egl/paseto"

	"github.com/determined-ai/determined/master/pkg/model"
)

// GenerateNotebookSessionToken signs a token for the JupyterLab of the task, owned by the user.
func GenerateNotebookSessionToken(
	userID model.UserID,
	taskID model.TaskID,
) (string, error) {
	notebookSession := &model.NotebookSession{
		UserID: userID,
		TaskID: taskID,
	}

	v2 := paseto.NewV2()
	token, err := v2.Sign(GetTokenKeys().PrivateKey, notebookSession, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate task authentication token: %w", err)
	}
	return token, nil
}
