import Button from 'hew/Button';
import Row from 'hew/Row';
import React from 'react';

import useGenericTaskActions, { GenericTaskActionTarget } from 'hooks/useGenericTaskActions';
import { GenericTaskState } from 'types';

export type { GenericTaskActionTarget } from 'hooks/useGenericTaskActions';

interface Props {
  canControl: boolean;
  onComplete?: () => void;
  task: GenericTaskActionTarget;
}

const NO_PERMISSION = 'You need to own this task or be an admin to control it.';

/*
 * The task page's actions. The confirmations and what each action does are shared with the generic
 * task menu (useGenericTaskActions).
 */
const GenericTaskActions: React.FC<Props> = ({ canControl, onComplete, task }: Props) => {
  const { canKill, canPause, canUnpause, isBusy, isUnpauseRetry, kill, killTree, pause, unpause } =
    useGenericTaskActions({ canControl, onComplete, task });

  const pauseTooltip = !canControl
    ? NO_PERMISSION
    : task.noPause !== false
      ? 'This task was not created pausable (det task create --pausable).'
      : task.state !== GenericTaskState.Active
        ? 'Only an active task can be paused.'
        : 'Stops this task and its pausable descendants.';

  const unpauseTooltip = !canControl
    ? NO_PERMISSION
    : isUnpauseRetry
      ? 'The last unpause failed. Retrying it on this task lets the master resume the rest.'
      : undefined;

  return (
    <Row>
      <Button
        data-testid="generic-task-pause"
        disabled={!canPause || isBusy}
        tooltip={pauseTooltip}
        onClick={pause}>
        Pause
      </Button>
      <Button
        data-testid="generic-task-unpause"
        disabled={!canUnpause || isBusy}
        tooltip={unpauseTooltip}
        onClick={unpause}>
        {isUnpauseRetry ? 'Retry Unpause' : 'Unpause'}
      </Button>
      <Button
        danger
        data-testid="generic-task-kill"
        disabled={!canKill || isBusy}
        tooltip={canControl ? undefined : NO_PERMISSION}
        onClick={kill}>
        Kill
      </Button>
      {task.parentId && (
        <Button
          danger
          data-testid="generic-task-kill-tree"
          disabled={!canControl || isBusy}
          tooltip={canControl ? 'Kills the whole tree from its root task.' : NO_PERMISSION}
          onClick={killTree}>
          Kill Tree
        </Button>
      )}
    </Row>
  );
};

export default GenericTaskActions;
