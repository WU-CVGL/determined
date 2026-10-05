import { useCallback } from 'react';

import { useLaunchForm } from 'hooks/useLaunchForm';
import { CommandTask, Workspace } from 'types';
import { isNtscLaunchType } from 'utils/ntscConfig';

interface Return {
  /** Opens the launch form with the task's type selected, starting from that task's config. */
  launchAgain: (task: CommandTask) => void;
  /** The launch form and the shell's launch result; render them once next to the list. */
  launchAgainModals: React.ReactNode;
}

/**
 * "Launch Again" for task lists: the launch form that the task action menu's Launch Again opens
 * (the same form as Launch JupyterLab and Launch Shell), and the result shown after a shell launch.
 */
export const useLaunchAgain = ({
  onLaunched,
  workspace,
}: {
  /** Called after a shell launch, for example to refresh the list. */
  onLaunched?: () => void;
  workspace?: Workspace;
}): Return => {
  const { launchFormModals, openLaunchForm } = useLaunchForm({
    onShellLaunched: onLaunched,
    workspace,
  });

  const launchAgain = useCallback(
    (task: CommandTask) => {
      if (isNtscLaunchType(task.type)) openLaunchForm(task.type, task);
    },
    [openLaunchForm],
  );

  return { launchAgain, launchAgainModals: launchFormModals };
};
