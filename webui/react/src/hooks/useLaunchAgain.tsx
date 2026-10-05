import { useModal } from 'hew/Modal';
import { useCallback, useState } from 'react';

import JupyterLabModalComponent from 'components/JupyterLabModal';
import ShellLaunchedModalComponent from 'components/ShellLaunchedModal';
import ShellModalComponent from 'components/ShellModal';
import { CommandResponse, CommandTask, CommandType, Workspace } from 'types';

interface Return {
  /** Opens the launch form of a shell or JupyterLab, starting from that task's config. */
  launchAgain: (task: CommandTask) => void;
  /** The launch forms and the shell's launch result; render them once next to the list. */
  launchAgainModals: React.ReactNode;
}

/**
 * "Launch Again" for task lists: the forms that the task action menu's Launch Again opens, and
 * the result shown after a shell launch.
 */
export const useLaunchAgain = ({
  onLaunched,
  workspace,
}: {
  /** Called after a shell launch, for example to refresh the list. */
  onLaunched?: () => void;
  workspace?: Workspace;
}): Return => {
  const JupyterLabAgainModal = useModal(JupyterLabModalComponent);
  const ShellAgainModal = useModal(ShellModalComponent);
  const ShellLaunchedModal = useModal(ShellLaunchedModalComponent);
  const [launchAgainTask, setLaunchAgainTask] = useState<CommandTask>();
  const [launchedShell, setLaunchedShell] = useState<CommandResponse>();
  const openJupyterLabAgain = JupyterLabAgainModal.open;
  const openShellAgain = ShellAgainModal.open;
  const openShellLaunched = ShellLaunchedModal.open;

  const launchAgain = useCallback(
    (task: CommandTask) => {
      setLaunchAgainTask(task);
      if (task.type === CommandType.Shell) openShellAgain();
      else if (task.type === CommandType.JupyterLab) openJupyterLabAgain();
    },
    [openJupyterLabAgain, openShellAgain],
  );

  const handleShellLaunched = useCallback(
    (response: CommandResponse) => {
      setLaunchedShell(response);
      openShellLaunched();
      onLaunched?.();
    },
    [onLaunched, openShellLaunched],
  );

  return {
    launchAgain,
    launchAgainModals: (
      <>
        {launchAgainTask?.type === CommandType.JupyterLab && (
          <JupyterLabAgainModal.Component
            initialTask={launchAgainTask}
            key={launchAgainTask.id}
            workspace={workspace}
          />
        )}
        {launchAgainTask?.type === CommandType.Shell && (
          <ShellAgainModal.Component
            initialTask={launchAgainTask}
            key={launchAgainTask.id}
            workspace={workspace}
            onLaunched={handleShellLaunched}
          />
        )}
        {launchedShell && <ShellLaunchedModal.Component response={launchedShell} />}
      </>
    ),
  };
};
