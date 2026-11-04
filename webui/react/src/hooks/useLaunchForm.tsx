import { useModal } from 'hew/Modal';
import { useCallback, useState } from 'react';

import NtscLaunchModalComponent from 'components/NtscLaunchModal';
import ShellLaunchedModalComponent from 'components/ShellLaunchedModal';
import { getTaskServices } from 'services/api';
import { V1TaskService } from 'services/api-ts-sdk';
import { CommandResponse, CommandTask, Workspace } from 'types';
import handleError from 'utils/error';

interface Return {
  /** The launch form and the shell's launch result; render them once. */
  launchFormModals: React.ReactNode;
  /** Opens the launch form. With a task ("Launch Again"), the form starts from that task's config. */
  openLaunchForm: (initialTask?: CommandTask) => void;
}

interface LaunchRequest {
  initialTask?: CommandTask;
}

interface LaunchedShell {
  response: CommandResponse;
  /** The launched shell's services, read once per launch; empty until read or when the read fails. */
  services: V1TaskService[];
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
  const [launchedShell, setLaunchedShell] = useState<LaunchedShell>();
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
      const taskId = response.command.id;
      setLaunchedShell({ response, services: [] });
      openShellLaunched();
      onShellLaunched?.();
      // The launch response does not list the task's services. They are fixed by its config, and
      // the launching user owns the task, so one read says which service buttons the dialog offers.
      // The dialog itself remounts on each open and close, so the read lives here.
      getTaskServices({ taskId })
        .then(({ services }) => {
          setLaunchedShell((prev) =>
            prev?.response.command.id === taskId ? { ...prev, services } : prev,
          );
        })
        .catch((e) => {
          handleError(e, {
            publicMessage: 'Unable to load the services of the task.',
            silent: true,
          });
        });
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
        {launchedShell && (
          <ShellLaunchedModal.Component
            response={launchedShell.response}
            services={launchedShell.services}
          />
        )}
      </>
    ),
    openLaunchForm,
  };
};
