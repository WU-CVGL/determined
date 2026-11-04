import Alert from 'hew/Alert';
import Button from 'hew/Button';
import CodeSample from 'hew/CodeSample';
import { Modal } from 'hew/Modal';
import Row from 'hew/Row';
import { Body, Label } from 'hew/Typography';
import React, { useEffect, useState } from 'react';

import Link from 'components/Link';
import useFeature from 'hooks/useFeature';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { paths } from 'routes/utils';
import { getTaskServices } from 'services/api';
import { V1LaunchWarning, V1TaskService } from 'services/api-ts-sdk';
import { CommandResponse, InteractiveTaskTab } from 'types';
import handleError from 'utils/error';
import { canOpenTaskService } from 'utils/task';
import { openInteractiveTask } from 'utils/wait';

interface Props {
  response: CommandResponse;
}

/** Shown after a shell launch: how to connect and where to follow the task. */
const ShellLaunchedModalComponent: React.FC<Props> = ({ response }: Props) => {
  const { command, warnings = [] } = response;
  const terminalEnabled = useFeature().isOn('shell_terminal');
  const resourcesEnabled = useTaskResourcesEnabled();
  // The launch response does not list the task's services. They are fixed by its config, and the
  // launching user owns the task, so one read says which service buttons to offer.
  const [services, setServices] = useState<V1TaskService[]>([]);
  useEffect(() => {
    let cancelled = false;
    setServices([]);
    getTaskServices({ taskId: command.id })
      .then((response) => {
        if (!cancelled) setServices(response.services);
      })
      .catch((e) => {
        handleError(e, { publicMessage: 'Unable to load the services of the task.', silent: true });
      });
    return () => {
      cancelled = true;
    };
  }, [command.id]);
  const openTab = (tab: InteractiveTaskTab) => () => openInteractiveTask(command.id, { tab });
  return (
    <Modal size="medium" title="Interactive Task Launched">
      {warnings.includes(V1LaunchWarning.CURRENTSLOTSEXCEEDED) && (
        <Alert
          message="The shell asks for more slots than are free right now, so it may wait in the queue."
          showIcon
          type="warning"
        />
      )}
      <Body>
        {command.name} ({command.id}) is starting. You can connect once its state is Running.
      </Body>
      {/* The launching user owns the task, and the task page's panes wait until it runs. */}
      <Row wrap>
        {terminalEnabled && (
          <Button size="large" type="primary" onClick={openTab(InteractiveTaskTab.Terminal)}>
            Open Terminal
          </Button>
        )}
        {canOpenTaskService(services, 'jupyter') && (
          <Button size="large" onClick={openTab(InteractiveTaskTab.JupyterLab)}>
            Open JupyterLab
          </Button>
        )}
        {canOpenTaskService(services, 'vscode') && (
          <Button size="large" onClick={openTab(InteractiveTaskTab.VSCode)}>
            Open Visual Studio Code
          </Button>
        )}
      </Row>
      <Label>Start an interactive SSH session in the terminal:</Label>
      <CodeSample text={`det shell open ${command.id}`} />
      <Row>
        <Link path={paths.taskLogs(command)}>View Logs</Link>
        {resourcesEnabled && <Link path={paths.taskResources(command.id)}>View Resources</Link>}
        <Link path={paths.taskList()}>View Tasks</Link>
      </Row>
    </Modal>
  );
};

export default ShellLaunchedModalComponent;
