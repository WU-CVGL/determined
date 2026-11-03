import { useModal } from 'hew/Modal';
import { useCallback, useState } from 'react';

import NtscLaunchModalComponent from 'components/NtscLaunchModal';
import ShellLaunchedModalComponent from 'components/ShellLaunchedModal';
import { CommandResponse, CommandTask, Workspace } from 'types';

interface Return {
  /** The launch form and the shell's launch result; render them once. */
  launchFormModals: React.ReactNode;
  /** Opens the launch form. With a task ("Launch Again"), the form starts from that task's config. */
  openLaunchForm: (initialTask?: CommandTask) => void;
}

interface LaunchRequest {
  initialTask?: CommandTask;
}

/** The shell launch form, and the shell's launch result (ShellLaunchedModal) after a launch. */
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
    (initialTask?: CommandTask) => {
      setRequest({ initialTask });
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
