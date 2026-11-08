package model

// NotebookSession is the claims of a JupyterLab token. ID is always 0.
type NotebookSession struct {
	ID     SessionID `json:"id"`
	TaskID TaskID    `json:"task_id"`
	UserID UserID    `json:"user_id"`
}

// NotebookSessionEnvVar is the environment variable name for notebook task tokens.
const NotebookSessionEnvVar = "DET_NOTEBOOK_TOKEN"
