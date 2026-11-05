import Button from 'hew/Button';
import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useRef, useState } from 'react';

import { V1TaskServiceState } from 'services/api-ts-sdk';
import { errorText, taskServiceUrl } from 'services/taskService';
import { taskServiceSettled } from 'utils/task';

import css from './TaskServicePane.module.scss';

interface Props {
  /** Whether the task has ended: its services no longer serve, whatever the last read said. */
  ended: boolean;
  /** The iframe's title. */
  label: string;
  service: string;
  /** The service's state, as the page that polls the task's services says. */
  state: V1TaskServiceState;
  taskId: string;
}

/** The states of a service that no longer serves: the page's poll reports them after READY. */
const GONE_STATES: V1TaskServiceState[] = [
  V1TaskServiceState.EXITED,
  V1TaskServiceState.UNAVAILABLE,
];

/**
 * An HTTP service of a task, such as JupyterLab, VS Code or TensorBoard, in an iframe. The
 * service's credential, if it has one, is read once, when the pane is shown and the service's
 * state has settled: the browser is logged in to the service and the iframe loads it. A failure
 * stays, as the service's own text, with Reconnect, which reads the credential and loads the
 * service once more (the service's process is left as it is): nothing is tried again on its own,
 * since code-server limits failed logins. A service that exits after it loaded shows its state the
 * same way. Once the task has ended, the pane shows the service's end, EXITED unless the service
 * could not run, and reads nothing.
 */
const TaskServicePane: React.FC<Props> = ({ ended, label, service, state, taskId }: Props) => {
  const [src, setSrc] = useState<string>();
  const [error, setError] = useState<string>();
  const [attempt, setAttempt] = useState(0);
  const started = useRef(false);

  useEffect(() => {
    if (ended || started.current || !taskServiceSettled(state)) return;
    started.current = true;
    taskServiceUrl(taskId, service)
      .then(setSrc)
      .catch((e) => setError(errorText(e)));
  }, [attempt, ended, service, state, taskId]);

  const reconnect = useCallback(() => {
    started.current = false;
    setError(undefined);
    setAttempt((a) => a + 1);
  }, []);

  if (ended) {
    const end = GONE_STATES.includes(state) ? state : V1TaskServiceState.EXITED;
    return (
      <div className={css.message}>
        <Message title={`${service}: ${end.replace('STATE_', '')}`} />
      </div>
    );
  }
  if (error) {
    return (
      <div className={css.message}>
        <Message action={<Button onClick={reconnect}>Reconnect</Button>} title={error} />
      </div>
    );
  }
  if (!src) return <Spinner center spinning />;
  if (GONE_STATES.includes(state)) {
    return (
      <div className={css.message}>
        <Message
          action={<Button onClick={reconnect}>Reconnect</Button>}
          title={`${service}: ${state.replace('STATE_', '')}`}
        />
      </div>
    );
  }
  return <iframe allowFullScreen className={css.frame} src={src} title={label} />;
};

export default TaskServicePane;
