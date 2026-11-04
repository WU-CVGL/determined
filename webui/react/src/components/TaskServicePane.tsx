import Button from 'hew/Button';
import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useRef, useState } from 'react';

import { V1TaskServiceState } from 'services/api-ts-sdk';
import { errorText, taskServiceUrl } from 'services/taskService';
import { taskServiceSettled } from 'utils/task';

import css from './TaskServicePane.module.scss';

interface Props {
  /** The iframe's title. */
  label: string;
  service: string;
  /** The service's state, as the page that polls the task's services says. */
  state: V1TaskServiceState;
  taskId: string;
}

/**
 * An HTTP service of a task, such as JupyterLab or VS Code, in an iframe. The service's credential
 * is read once, when the pane is shown and the service's state has settled: the browser is logged
 * in to the service and the iframe loads it. A failure stays, as the service's own text, with
 * Retry for one more attempt: nothing is tried again on its own, since code-server limits failed
 * logins.
 */
const TaskServicePane: React.FC<Props> = ({ label, service, state, taskId }: Props) => {
  const [src, setSrc] = useState<string>();
  const [error, setError] = useState<string>();
  const [attempt, setAttempt] = useState(0);
  const started = useRef(false);

  useEffect(() => {
    if (started.current || !taskServiceSettled(state)) return;
    started.current = true;
    taskServiceUrl(taskId, service)
      .then(setSrc)
      .catch((e) => setError(errorText(e)));
  }, [attempt, service, state, taskId]);

  const retry = useCallback(() => {
    started.current = false;
    setError(undefined);
    setAttempt((a) => a + 1);
  }, []);

  if (error) {
    return (
      <div className={css.message}>
        <Message action={<Button onClick={retry}>Retry</Button>} title={error} />
      </div>
    );
  }
  if (!src) return <Spinner center spinning />;
  return <iframe allowFullScreen className={css.frame} src={src} title={label} />;
};

export default TaskServicePane;
