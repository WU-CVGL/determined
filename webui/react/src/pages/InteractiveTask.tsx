import Button from 'hew/Button';
import { Settings, settingsConfigForTask } from 'hew/LogViewer/LogViewerSelect.settings';
import Message from 'hew/Message';
import Pivot, { TabItem } from 'hew/Pivot';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
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
  canOpenTaskService,
  commandTypeOfTask,
  taskPhase,
  taskServiceSettled,
} from 'utils/task';
import { waitPageUrl } from 'utils/wait';

import css from './InteractiveTask.module.scss';

type Params = {
  taskId: string;
};

const POLL_MS = 2000;
/** How often the services are read once they have settled, while the task runs. */
const SERVICES_SLOW_POLL_MS = 10000;
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
 * A task's page: its services as tabs (a terminal, JupyterLab, VS Code, TensorBoard; a
 * TensorBoard task's own page), then its logs and resources. One poll of the task feeds the title
 * and the panes, which stay mounted once shown so that their sessions keep their state.
 */
export const InteractiveTask: React.FC = () => {
  const { taskId = '' } = useParams<Params>();
  const [searchParams, setSearchParams] = useSearchParams();
  const { actions: uiActions, ui } = useUI();
  const terminalEnabled = useFeature().isOn('shell_terminal');
  const resourcesEnabled = useTaskResourcesEnabled();
  const [task, setTask] = useState<TaskItem>();
  const [taskServices, setTaskServices] = useState<TaskServices>();
  // Why the services have not been read yet, once the page has given up reading them.
  const [servicesError, setServicesError] = useState<string>();
  const [retrying, setRetrying] = useState(false);
  const retryServicesRef = useRef<() => Promise<void>>();
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

  // The one poll of the task, until it ends, and of its services: on every tick while one of them
  // has not settled, then every SERVICES_SLOW_POLL_MS while the task runs, so that a service that
  // exits after it was ready shows its state, and once more when the task has ended. The set of
  // services is fixed, and a pane opens its service once it is ready. The services are not read
  // while the page is hidden; showing it reads them at once. One tick at a time: the next is
  // scheduled once the current one has answered, and one read of the services at a time, so that
  // an older answer never lands after a newer one.
  useEffect(() => {
    let cancelled = false;
    let failures = 0;
    let loaded = false;
    let serviceFailures = 0;
    let servicesLoaded = false;
    // Whether the page has given up reading the services until Retry: the task's own tabs are
    // shown without them.
    let servicesGivenUp = false;
    // Whether the services API does not know the task, which then has no services to read again.
    let servicesUnknown = false;
    let servicesSettled = false;
    // The read of the services in flight, if any.
    let servicesRead: Promise<void> | undefined;
    // When the last read of the services started, and whether the read after the task's end ran.
    let servicesReadAt = 0;
    let endRead = false;
    let ended = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
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
        if (loaded) return undefined;
        // A task the master does not know is not asked for again; another failure may pass.
        if (isNotFound(e as Error) || ++failures >= MAX_POLL_FAILURES) {
          setError(text);
          return 'gone';
        }
        return undefined;
      }
    };
    // A failed read keeps the last services, if any, and is tried again on the next tick; before
    // the first answer, the page gives up after a few failures and shows the last one with Retry.
    // A task the services API does not know, such as a generic task, has none.
    const pollServices = async () => {
      try {
        const response = await getTaskServices({ taskId });
        if (cancelled) return;
        servicesLoaded = true;
        servicesGivenUp = false;
        setServicesError(undefined);
        setTaskServices({
          owner: { userId: response.userId, workspaceId: response.workspaceId },
          services: response.services,
        });
        servicesSettled = response.services.every((s) => taskServiceSettled(s.state));
      } catch (e) {
        if (cancelled) return;
        const text = errorText(e);
        handleError(e, { publicMessage: 'Unable to load the services of the task.', silent: true });
        if (isNotFound(e as Error)) {
          servicesLoaded = true;
          servicesGivenUp = false;
          servicesUnknown = true;
          setServicesError(undefined);
          setTaskServices({ services: [] });
          servicesSettled = true;
        } else if (!servicesLoaded && (servicesGivenUp || ++serviceFailures >= MAX_POLL_FAILURES)) {
          servicesGivenUp = true;
          setServicesError(text);
        }
      }
    };
    // One read at a time; a read asked for while another runs waits for that one.
    const readServices = (): Promise<void> => {
      if (servicesRead) return servicesRead;
      servicesReadAt = Date.now();
      servicesRead = pollServices().finally(() => {
        servicesRead = undefined;
      });
      return servicesRead;
    };
    // Whether the page reads the services on this tick.
    const servicesDue = (): boolean => {
      if (servicesGivenUp || servicesUnknown || servicesRead || document.hidden) return false;
      if (!servicesSettled) return true;
      return !ended && Date.now() - servicesReadAt >= SERVICES_SLOW_POLL_MS;
    };
    // One more read, on request; a success resumes the reads of the poll while it runs.
    retryServicesRef.current = async () => {
      setRetrying(true);
      await readServices();
      setRetrying(false);
    };
    const poll = async () => {
      const [outcome] = await Promise.all([pollTask(), servicesDue() ? readServices() : undefined]);
      if (cancelled || outcome === 'gone') return;
      if (outcome === 'ended') {
        ended = true;
        // The services' states after the end, once, from a read that starts after the end.
        if (!endRead && !servicesGivenUp && !servicesUnknown) {
          endRead = true;
          await servicesRead;
          if (cancelled) return;
          await readServices();
          if (cancelled) return;
        }
        if (servicesSettled || servicesGivenUp) return;
      }
      timer = setTimeout(() => void poll(), POLL_MS);
    };
    const onVisibilityChange = () => {
      if (!document.hidden && !ended && !servicesGivenUp && !servicesUnknown) void readServices();
    };
    document.addEventListener('visibilitychange', onVisibilityChange);
    void poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisibilityChange);
      retryServicesRef.current = undefined;
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
    if (canOpenTaskService(services, 'jupyter')) keys.push(InteractiveTaskTab.JupyterLab);
    if (canOpenTaskService(services, 'vscode')) keys.push(InteractiveTaskTab.VSCode);
    if (
      (command?.type === CommandType.TensorBoard && command.serviceAddress) ||
      canOpenTaskService(services, 'tensorboard')
    ) {
      keys.push(InteractiveTaskTab.TensorBoard);
    }
    keys.push(InteractiveTaskTab.Logs);
    if (resourcesEnabled || requestedTab === InteractiveTaskTab.Resources) {
      keys.push(InteractiveTaskTab.Resources);
    }
    return keys;
  }, [command, requestedTab, resourcesEnabled, services, terminalEnabled]);

  const activeKey = tabKeys.find((key) => key === requestedTab) ?? tabKeys[0];

  // Without a tab in the URL, the page stays on the tab it shows when a Retry adds the service
  // tabs before it; a tab in the URL is the one asked for.
  const retryServices = useCallback(() => {
    if (!requestedTab) selectTab(activeKey);
    void retryServicesRef.current?.();
  }, [activeKey, requestedTab, selectTab]);

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
          // A TensorBoard task's own page, or a shell's tensorboard service.
          if (command?.type === CommandType.TensorBoard) {
            return (
              <iframe
                allowFullScreen
                className={css.frame}
                src={waitPageUrl(command)}
                title="TensorBoard"
              />
            );
          }
          return (
            <TaskServicePane
              label="TensorBoard"
              service="tensorboard"
              state={serviceState('tensorboard')}
              taskId={taskId}
            />
          );
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
  // The state for the task bar's Kill: ended by the phase, else the allocation's, queued without
  // one or while it waits (a live state, as the task tables show it).
  let killState: CommandState | undefined;
  if (task && phase === 'ended') killState = CommandState.Terminated;
  else if (task) killState = state && state !== CommandState.Waiting ? state : CommandState.Queued;

  let content: React.ReactNode;
  if (error) {
    content = (
      <div className={css.message}>
        <Message title={error} />
      </div>
    );
  } else if (!task || !lookedUp || (!taskServices && !servicesError)) {
    // Also the lookup: a TensorBoard's tab comes from it.
    content = <Spinner center spinning />;
  } else {
    content = (
      <>
        {servicesError && (
          <div className={css.notice}>
            <Message
              action={
                <Button disabled={retrying} onClick={retryServices}>
                  Retry
                </Button>
              }
              title={servicesError}
            />
          </div>
        )}
        <div className={css.tabs}>
          <Pivot activeKey={activeKey} items={items} onChange={selectTab} />
        </div>
      </>
    );
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
            state={killState}
            type={commandType}
          />
        </div>
        <div className={css.contentContainer}>{content}</div>
      </div>
    </>
  );
};

export default InteractiveTask;
