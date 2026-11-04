import { Settings, settingsConfigForTask } from 'hew/LogViewer/LogViewerSelect.settings';
import Message from 'hew/Message';
import Pivot, { TabItem } from 'hew/Pivot';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Helmet } from 'react-helmet-async';
import { useParams, useSearchParams } from 'react-router-dom';

import ShellTerminalPane from 'components/ShellTerminalPane';
import TaskBar from 'components/TaskBar';
import TaskResourcesPanel from 'components/TaskResourcesPanel';
import TaskServicePane from 'components/TaskServicePane';
import useUI from 'components/ThemeProvider';
import useFeature from 'hooks/useFeature';
import { useSettings } from 'hooks/useSettings';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { TaskLogsViewer } from 'pages/TaskLogs';
import { getCommand, getShells, getTask, getTaskServices, getTensorBoard } from 'services/api';
import { V1TaskService } from 'services/api-ts-sdk';
import { errorText } from 'services/taskService';
import { CommandState, CommandTask, CommandType, InteractiveTaskTab, TaskItem } from 'types';
import handleError, { ErrorLevel, ErrorType, handleWarning } from 'utils/error';
import { canOpenShellTerminal, commandTypeOfTask, hasTaskService, taskPhase } from 'utils/task';
import { waitPageUrl } from 'utils/wait';

import css from './InteractiveTask.module.scss';

type Params = {
  taskId: string;
};

const POLL_MS = 2000;
const MAX_POLL_FAILURES = 5;
const DEFAULT_PAGE_TITLE = 'Tasks - Determined';

const TAB_LABELS: Record<InteractiveTaskTab, string> = {
  [InteractiveTaskTab.Terminal]: 'Terminal',
  [InteractiveTaskTab.JupyterLab]: 'JupyterLab',
  [InteractiveTaskTab.VSCode]: 'VS Code',
  [InteractiveTaskTab.TensorBoard]: 'TensorBoard',
  [InteractiveTaskTab.Logs]: 'Logs',
  [InteractiveTaskTab.Resources]: 'Resources',
};

const getTitleState = (commandState?: CommandState, taskName?: string): string => {
  if (!commandState) {
    return DEFAULT_PAGE_TITLE;
  }
  const commandStateTitleMap = {
    [CommandState.Waiting]: 'Waiting',
    [CommandState.Pulling]: 'Pulling',
    [CommandState.Queued]: 'Queued',
    [CommandState.Running]: taskName || DEFAULT_PAGE_TITLE,
    [CommandState.Terminating]: 'Terminating',
    [CommandState.Terminated]: 'Terminated',
    [CommandState.Starting]: 'Starting',
  };
  const title = commandStateTitleMap[commandState];
  if (commandState !== CommandState.Terminated && commandState !== CommandState.Running) {
    return title + '...';
  }
  return title;
};

/** The owner of a task and the services it declares, as the services API reports them. */
interface TaskServices {
  owner?: { userId: number; workspaceId: number };
  services: V1TaskService[];
}

/* The task's name, pool and, for a TensorBoard, address: from the lookup of its type, if any. */
const lookupCommand = async (
  taskId: string,
  type?: CommandType,
): Promise<CommandTask | undefined> => {
  switch (type) {
    case CommandType.Command:
      return await getCommand({ commandId: taskId });
    case CommandType.TensorBoard:
      return await getTensorBoard({ commandId: taskId });
    case CommandType.Shell:
      // The shell list has no private keys, unlike a shell's own lookup.
      return (await getShells({})).find((shell) => shell.id === taskId);
    default:
      return undefined;
  }
};

const LogsPane: React.FC<{ taskId: string }> = ({ taskId }) => {
  const taskSettingsConfig = useMemo(() => settingsConfigForTask(taskId), [taskId]);
  const { resetSettings, settings, updateSettings } = useSettings<Settings>(taskSettingsConfig);
  return (
    <TaskLogsViewer
      resetSettings={resetSettings}
      settings={settings}
      taskId={taskId}
      updateSettings={updateSettings}
    />
  );
};

/**
 * A task's page: its services as tabs (a terminal, JupyterLab, VS Code; TensorBoard's own page),
 * then its logs and resources. One poll of the task feeds the title and the panes, which stay
 * mounted once shown so that their sessions keep their state.
 */
export const InteractiveTask: React.FC = () => {
  const { taskId = '' } = useParams<Params>();
  const [searchParams, setSearchParams] = useSearchParams();
  const { actions: uiActions, ui } = useUI();
  const terminalEnabled = useFeature().isOn('shell_terminal');
  const resourcesEnabled = useTaskResourcesEnabled();
  const [task, setTask] = useState<TaskItem>();
  const [taskServices, setTaskServices] = useState<TaskServices>();
  const [command, setCommand] = useState<CommandTask>();
  const [error, setError] = useState<string>();

  const requestedTab = searchParams.get('tab');
  const slotsExceeded = searchParams.get('currentSlotsExceeded') === 'true';

  useEffect(() => {
    uiActions.hideChrome();
    return uiActions.showChrome;
  }, [uiActions]);

  useEffect(() => {
    if (slotsExceeded) {
      handleWarning({
        level: ErrorLevel.Warn,
        publicMessage:
          'The requested job requires more slots than currently available. You may need to increase cluster resources in order for the job to run.',
        publicSubject: 'Current Slots Exceeded',
        silent: false,
        type: ErrorType.Server,
      });
    }
  }, [slotsExceeded]);

  // The one poll of the task, until it ends.
  useEffect(() => {
    let cancelled = false;
    let failures = 0;
    let loaded = false;
    const timer = setInterval(() => void poll(), POLL_MS);
    const poll = async () => {
      try {
        const response = await getTask({ taskId });
        if (cancelled) return;
        failures = 0;
        if (!response) {
          clearInterval(timer);
          if (!loaded) setError(`Task ${taskId} not found.`);
          return;
        }
        loaded = true;
        setTask(response);
        if (taskPhase(response) === 'ended') clearInterval(timer);
      } catch (e) {
        if (cancelled) return;
        // The server's text, before the handler puts its own on the error.
        const text = errorText(e);
        handleError(e, { publicMessage: 'Unable to load the task.', silent: true });
        if (++failures >= MAX_POLL_FAILURES) {
          clearInterval(timer);
          if (!loaded) setError(text);
        }
      }
    };
    void poll();
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [taskId]);

  // The services and the owner, once: the set of services is fixed, and a pane reads a service's
  // state when it opens.
  useEffect(() => {
    let cancelled = false;
    getTaskServices({ taskId })
      .then((response) => {
        if (cancelled) return;
        setTaskServices({
          owner: { userId: response.userId, workspaceId: response.workspaceId },
          services: response.services,
        });
      })
      .catch((e) => {
        if (cancelled) return;
        handleError(e, { publicMessage: 'Unable to load the services of the task.', silent: true });
        setTaskServices({ services: [] });
      });
    return () => {
      cancelled = true;
    };
  }, [taskId]);

  const commandType = commandTypeOfTask(task?.taskType);

  // The name and pool, from the lookup of the task's type; an ended task has none.
  useEffect(() => {
    if (!task) return;
    let cancelled = false;
    lookupCommand(taskId, commandType)
      .then((response) => {
        if (!cancelled) setCommand(response);
      })
      .catch((e) => {
        if (!cancelled) handleError(e, { publicMessage: 'Unable to load the task.', silent: true });
      });
    return () => {
      cancelled = true;
    };
    // Once the task's type is known.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskId, commandType]);

  const selectTab = useCallback(
    (tab: string) =>
      setSearchParams(
        (params) => {
          params.set('tab', tab);
          return params;
        },
        { replace: true },
      ),
    [setSearchParams],
  );

  const name = command?.name || taskId;
  const phase = task ? taskPhase(task) : 'waiting';
  const services = useMemo(() => taskServices?.services ?? [], [taskServices]);

  // The tabs the task has: its services first, then the logs and resources of every task.
  const tabKeys = useMemo(() => {
    const keys: InteractiveTaskTab[] = [];
    if (terminalEnabled && canOpenShellTerminal(services)) keys.push(InteractiveTaskTab.Terminal);
    if (hasTaskService(services, 'jupyter')) keys.push(InteractiveTaskTab.JupyterLab);
    if (hasTaskService(services, 'vscode')) keys.push(InteractiveTaskTab.VSCode);
    if (command?.type === CommandType.TensorBoard && command.serviceAddress) {
      keys.push(InteractiveTaskTab.TensorBoard);
    }
    keys.push(InteractiveTaskTab.Logs);
    if (resourcesEnabled || requestedTab === InteractiveTaskTab.Resources) {
      keys.push(InteractiveTaskTab.Resources);
    }
    return keys;
  }, [command, requestedTab, resourcesEnabled, services, terminalEnabled]);

  const activeKey = tabKeys.find((key) => key === requestedTab) ?? tabKeys[0];

  const items: TabItem[] = useMemo(() => {
    if (!task) return [];
    const pane = (key: InteractiveTaskTab): React.ReactNode => {
      switch (key) {
        case InteractiveTaskTab.Terminal:
          return <ShellTerminalPane active={activeKey === key} phase={phase} shellId={taskId} />;
        case InteractiveTaskTab.JupyterLab:
          return (
            <TaskServicePane label="JupyterLab" phase={phase} service="jupyter" taskId={taskId} />
          );
        case InteractiveTaskTab.VSCode:
          return <TaskServicePane label="VS Code" phase={phase} service="vscode" taskId={taskId} />;
        case InteractiveTaskTab.TensorBoard:
          return command ? (
            <iframe
              allowFullScreen
              className={css.frame}
              src={waitPageUrl(command)}
              title="TensorBoard"
            />
          ) : null;
        case InteractiveTaskTab.Logs:
          return <LogsPane taskId={taskId} />;
        case InteractiveTaskTab.Resources:
          return (
            <TaskResourcesPanel endTime={task.endTime} startTime={task.startTime} taskId={taskId} />
          );
      }
    };
    return tabKeys.map((key) => ({ children: pane(key), key, label: TAB_LABELS[key] }));
  }, [activeKey, command, phase, tabKeys, task, taskId]);

  const state = task?.allocations[0]?.state;
  const title = ui.isPageHidden ? getTitleState(state, name) : name;

  let content: React.ReactNode;
  if (error) {
    content = (
      <div className={css.message}>
        <Message title={error} />
      </div>
    );
  } else if (!task || !taskServices) {
    content = <Spinner center spinning />;
  } else {
    content = <Pivot activeKey={activeKey} items={items} onChange={selectTab} />;
  }

  return (
    <>
      <Helmet defer={false}>
        <title>{title}</title>
      </Helmet>
      <div className={css.base}>
        <div className={css.barContainer}>
          <TaskBar
            handleViewLogsClick={() => selectTab(InteractiveTaskTab.Logs)}
            id={taskId}
            name={name}
            owner={taskServices?.owner}
            resourcePool={command?.resourcePool ?? ''}
            type={commandType}
          />
        </div>
        <div className={css.contentContainer}>{content}</div>
      </div>
    </>
  );
};

export default InteractiveTask;
