import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import React, { useEffect, useRef, useState } from 'react';

import { errorText, taskServiceUrl } from 'services/taskService';
import { TaskPhase } from 'utils/task';

import css from './TaskServicePane.module.scss';

interface Props {
  /** The iframe's title. */
  label: string;
  phase: TaskPhase;
  service: string;
  taskId: string;
}

/**
 * An HTTP service of a task, such as JupyterLab or VS Code, in an iframe. The service's credential
 * is read once, when the pane first shows and the task is no longer waiting: the browser is logged
 * in to the service and the iframe loads it. A failure stays, as the service's own text.
 */
const TaskServicePane: React.FC<Props> = ({ label, phase, service, taskId }: Props) => {
  const [src, setSrc] = useState<string>();
  const [error, setError] = useState<string>();
  const started = useRef(false);

  useEffect(() => {
    if (phase === 'waiting' || started.current) return;
    started.current = true;
    taskServiceUrl(taskId, service)
      .then(setSrc)
      .catch((e) => setError(errorText(e)));
  }, [phase, service, taskId]);

  if (error) {
    return (
      <div className={css.message}>
        <Message title={error} />
      </div>
    );
  }
  if (!src) return <Spinner center spinning />;
  return <iframe allowFullScreen className={css.frame} src={src} title={label} />;
};

export default TaskServicePane;
