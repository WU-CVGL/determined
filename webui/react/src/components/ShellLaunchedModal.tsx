import Alert from 'hew/Alert';
import Button from 'hew/Button';
import CodeSample from 'hew/CodeSample';
import Column from 'hew/Column';
import { Modal } from 'hew/Modal';
import Row from 'hew/Row';
import { Body, Label } from 'hew/Typography';
import React from 'react';

import Link from 'components/Link';
import useFeature from 'hooks/useFeature';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { paths } from 'routes/utils';
import { V1LaunchWarning, V1TaskService } from 'services/api-ts-sdk';
import { CommandResponse, InteractiveTaskTab } from 'types';
import { canOpenTaskService } from 'utils/task';
import { openInteractiveTask } from 'utils/wait';

interface Props {
  response: CommandResponse;
  /** The launched task's services, read by the launching hook; a button per service it offers. */
  services: V1TaskService[];
}

/** Shown after a shell launch: how to connect and where to follow the task. */
const ShellLaunchedModalComponent: React.FC<Props> = ({ response, services }: Props) => {
  const { command, warnings = [] } = response;
  const terminalEnabled = useFeature().isOn('shell_terminal');
  const resourcesEnabled = useTaskResourcesEnabled();
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
      <Column>
        {terminalEnabled && (
          <Button block size="large" type="primary" onClick={openTab(InteractiveTaskTab.Terminal)}>
            Open Terminal
          </Button>
        )}
        {canOpenTaskService(services, 'jupyter') && (
          <Button
            block
            size="large"
            type="primary"
            onClick={openTab(InteractiveTaskTab.JupyterLab)}>
            Open JupyterLab
          </Button>
        )}
        {canOpenTaskService(services, 'vscode') && (
          <Button block size="large" type="primary" onClick={openTab(InteractiveTaskTab.VSCode)}>
            Open VS Code
          </Button>
        )}
        {canOpenTaskService(services, 'tensorboard') && (
          <Button block size="large" onClick={openTab(InteractiveTaskTab.TensorBoard)}>
            Open TensorBoard
          </Button>
        )}
      </Column>
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
