import { useModal } from 'hew/Modal';
import { useCallback, useState } from 'react';

import NtscLaunchModalComponent from 'components/NtscLaunchModal';
import ShellLaunchedModalComponent from 'components/ShellLaunchedModal';
import { CommandResponse, CommandTask, Workspace } from 'types';
import { NtscLaunchType } from 'utils/ntscConfig';

interface Return {
  /** The launch form and the shell's launch result; render them once. */
  launchFormModals: React.ReactNode;
  /**
   * Opens the launch form with the given task type selected. With a task ("Launch Again"), the
   * form starts from that task's config.
   */
  openLaunchForm: (type: NtscLaunchType, initialTask?: CommandTask) => void;
}

interface LaunchRequest {
  initialTask?: CommandTask;
  type: NtscLaunchType;
}

/**
 * The launch form for JupyterLab and shells, and what follows a launch. The user can switch the
 * task type in the form, so every entry point shows the shell's launch result (ShellLaunchedModal)
 * after a shell launch; a JupyterLab launch opens the notebook's wait page itself.
 */
export const useLaunchForm = ({
  onShellLaunched,
  workspace,
}: {
  /** Called after a shell launch, for example to refresh a list. */
  onShellLaunched?: () => void;
  workspace?: Workspace;
} = {}): Return => {
  const LaunchModal = useModal(NtscLaunchModalComponent);
  const ShellLaunchedModal = useModal(ShellLaunchedModalComponent);
  // Unset until the form is first opened, so that a page does not mount a closed form.
  const [request, setRequest] = useState<LaunchRequest>();
  const [launchedShell, setLaunchedShell] = useState<CommandResponse>();
  const openLaunchModal = LaunchModal.open;
  const openShellLaunched = ShellLaunchedModal.open;

  const openLaunchForm = useCallback(
    (type: NtscLaunchType, initialTask?: CommandTask) => {
      setRequest({ initialTask, type });
      openLaunchModal();
    },
    [openLaunchModal],
  );

  const handleShellLaunched = useCallback(
    (response: CommandResponse) => {
      setLaunchedShell(response);
      openShellLaunched();
      onShellLaunched?.();
    },
    [onShellLaunched, openShellLaunched],
  );

  return {
    launchFormModals: (
      <>
        {request && (
          <LaunchModal.Component
            initialTask={request.initialTask}
            initialType={request.type}
            key={request.initialTask?.id}
            workspace={workspace}
            onShellLaunched={handleShellLaunched}
          />
        )}
        {launchedShell && <ShellLaunchedModal.Component response={launchedShell} />}
      </>
    ),
    openLaunchForm,
  };
};
