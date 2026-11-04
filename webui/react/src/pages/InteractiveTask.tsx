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
import { V1TaskService, V1TaskServiceState } from 'services/api-ts-sdk';
import { errorText } from 'services/taskService';
import { CommandState, CommandTask, CommandType, InteractiveTaskTab, TaskItem } from 'types';
import handleError, { ErrorLevel, ErrorType, handleWarning } from 'utils/error';
import { isNotFound } from 'utils/service';
import {
  canOpenShellTerminal,
  commandTypeOfTask,
  hasTaskService,
  taskPhase,
  taskServiceSettled,
} from 'utils/task';
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
    <div className={css.pane}>
      <TaskLogsViewer
        resetSettings={resetSettings}
        settings={settings}
        taskId={taskId}
        updateSettings={updateSettings}
      />
    </div>
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
  // Whether the lookup of the task's name and pool has answered, with or without a record.
  const [lookedUp, setLookedUp] = useState(false);
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

  // The one poll of the task, until it ends, and of its services while one of them has not
  // settled: the set of services is fixed, and a pane opens its service once it is ready.
  useEffect(() => {
    let cancelled = false;
    let failures = 0;
    let loaded = false;
    let servicesSettled = false;
    const timer = setInterval(() => void poll(), POLL_MS);
    // Whether the task has ended, or the page gives up on it.
    const pollTask = async (): Promise<'ended' | 'gone' | undefined> => {
      try {
        const response = await getTask({ taskId });
        if (cancelled) return 'gone';
        failures = 0;
        if (!response) {
          if (!loaded) setError(`Task ${taskId} not found.`);
          return 'gone';
        }
        loaded = true;
        setTask(response);
        return taskPhase(response) === 'ended' ? 'ended' : undefined;
      } catch (e) {
        if (cancelled) return 'gone';
        // The server's text, before the handler puts its own on the error.
        const text = errorText(e);
        handleError(e, { publicMessage: 'Unable to load the task.', silent: true });
        if (++failures >= MAX_POLL_FAILURES) {
          if (!loaded) setError(text);
          return 'gone';
        }
        return undefined;
      }
    };
    // A failed read keeps the last services, if any, and is tried again on the next tick; a task
    // the services API does not know, such as a generic task, has none.
    const pollServices = async () => {
      try {
        const response = await getTaskServices({ taskId });
        if (cancelled) return;
        setTaskServices({
          owner: { userId: response.userId, workspaceId: response.workspaceId },
          services: response.services,
        });
        servicesSettled = response.services.every((s) => taskServiceSettled(s.state));
      } catch (e) {
        if (cancelled) return;
        handleError(e, { publicMessage: 'Unable to load the services of the task.', silent: true });
        if (isNotFound(e as Error)) {
          setTaskServices({ services: [] });
          servicesSettled = true;
        }
      }
    };
    const poll = async () => {
      const [outcome] = await Promise.all([
        pollTask(),
        servicesSettled ? undefined : pollServices(),
      ]);
      if (outcome === 'gone' || (outcome === 'ended' && servicesSettled)) clearInterval(timer);
    };
    void poll();
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [taskId]);

  const commandType = commandTypeOfTask(task?.taskType);
  const loaded = !!task;

  // The name and pool, from the lookup of the task's type; an ended task has none.
  useEffect(() => {
    if (!loaded) return;
    let cancelled = false;
    lookupCommand(taskId, commandType)
      .then((response) => {
        if (!cancelled) setCommand(response);
      })
      .catch((e) => {
        if (!cancelled) handleError(e, { publicMessage: 'Unable to load the task.', silent: true });
      })
      .finally(() => {
        if (!cancelled) setLookedUp(true);
      });
    return () => {
      cancelled = true;
    };
  }, [taskId, commandType, loaded]);

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
    const serviceState = (name: string): V1TaskServiceState =>
      services.find((s) => s.name === name)?.state ?? V1TaskServiceState.UNSPECIFIED;
    const pane = (key: InteractiveTaskTab): React.ReactNode => {
      switch (key) {
        case InteractiveTaskTab.Terminal:
          return <ShellTerminalPane active={activeKey === key} phase={phase} shellId={taskId} />;
        case InteractiveTaskTab.JupyterLab:
          return (
            <TaskServicePane
              label="JupyterLab"
              service="jupyter"
              state={serviceState('jupyter')}
              taskId={taskId}
            />
          );
        case InteractiveTaskTab.VSCode:
          return (
            <TaskServicePane
              label="VS Code"
              service="vscode"
              state={serviceState('vscode')}
              taskId={taskId}
            />
          );
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
  }, [activeKey, command, phase, services, tabKeys, task, taskId]);

  const state = task?.allocations[0]?.state;
  const title = ui.isPageHidden ? getTitleState(state, name) : name;

  let content: React.ReactNode;
  if (error) {
    content = (
      <div className={css.message}>
        <Message title={error} />
      </div>
    );
  } else if (!task || !taskServices || !lookedUp) {
    // Also the lookup: a TensorBoard's tab comes from it.
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
