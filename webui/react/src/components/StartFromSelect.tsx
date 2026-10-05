import dayjs from 'dayjs';
import Button from 'hew/Button';
import Select, { OptGroup, Option, SelectValue } from 'hew/Select';
import { Loadable } from 'hew/utils/loadable';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import {
  getJupyterLabConfig,
  getJupyterLabs,
  getShellConfig,
  getShells,
  getTaskTemplates,
} from 'services/api';
import { GetShellsParams } from 'services/types';
import userStore from 'stores/users';
import { CommandTask, CommandType, RawJson, Template } from 'types';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import {
  clearAllLaunchHistory,
  listAllLaunchHistory,
  TypedLaunchHistoryEntry,
} from 'utils/launchHistory';
import { formFieldsFromConfig, isNtscLaunchType, NTSC_LAUNCH_TYPE_LABELS } from 'utils/ntscConfig';
import { useObservable } from 'utils/observable';
import { isNotFound } from 'utils/service';
import { capitalize } from 'utils/string';

/** How many of the user's recent tasks of each type the master is asked for, and how many are listed. */
export const RECENT_TASK_LIMIT = 20;

/** What the launch form starts from once a "Start from" item is picked. */
export type StartFrom =
  | { kind: 'blank' }
  | { kind: 'template'; template: Template }
  | { kind: 'config'; config: RawJson; redactedEnv: string[]; workspaceId: number };

const TASK_PREFIX = 'task:';
const LOCAL_PREFIX = 'local:';
const TEMPLATE_PREFIX = 'template:';

export const startFromTaskValue = (taskId: string): string => `${TASK_PREFIX}${taskId}`;
export const startFromTemplateValue = (name: string): string => `${TEMPLATE_PREFIX}${name}`;
export const startFromLocalValue = (id: string): string => `${LOCAL_PREFIX}${id}`;

/** The template name a "Start from" value points at, if it is a template. */
export const templateNameFromStartFrom = (value?: string): string | undefined =>
  value?.startsWith(TEMPLATE_PREFIX) ? value.slice(TEMPLATE_PREFIX.length) : undefined;

const formatTime = (time: number | string): string => dayjs(time).format('MMM D, HH:mm');

const typeLabel = (type: CommandType): string =>
  isNtscLaunchType(type) ? NTSC_LAUNCH_TYPE_LABELS[type] : capitalize(type);

const taskLabel = (task: CommandTask): string =>
  [
    typeLabel(task.type),
    task.name,
    capitalize(task.state.toLowerCase()),
    formatTime(task.startTime),
  ].join(' · ');

const localLabel = (entry: TypedLaunchHistoryEntry): string => {
  const { name, pool, slots = 1 } = formFieldsFromConfig(entry.config);
  return [
    typeLabel(entry.type),
    name || 'Unnamed',
    `${pool ?? 'default pool'}, ${slots} slot${slots === 1 ? '' : 's'}`,
    formatTime(entry.savedAt),
  ].join(' · ');
};

interface Props {
  /** Workspaces the user may launch in; items from other workspaces are disabled. */
  allowedWorkspaceIds: number[];
  /**
   * Whether to make the first selection (initialTask, or else defaultTemplate).
   * The modal turns it off through onAutoSelected, so that remounting the
   * picker (after the full-config mode) does not select again.
   */
  autoSelect: boolean;
  /** Template to preselect, from the user's last launch. */
  defaultTemplate?: string;
  /** Set by Form.Item, so that the field's label points at the select. */
  id?: string;
  /** "Launch Again": the task to start from as soon as the picker mounts. */
  initialTask?: CommandTask;
  /**
   * The recent task whose config is being fetched. The modal holds it, so that
   * it can keep Launch and the full config disabled until onResolve is called
   * for the picked item.
   */
  loadingTaskId?: string;
  /** When the modal is locked to a workspace. */
  lockedWorkspaceId?: number;
  onAutoSelected: () => void;
  onChange?: (value?: string) => void;
  onLoadingTaskChange: (taskId?: string) => void;
  onResolve: (start: StartFrom) => void;
  value?: string;
}

/** One "Start from" item as the dropdown lists it. */
interface StartFromItem {
  disabled?: boolean;
  label: string;
  value: string;
}

interface StartFromGroup {
  items: StartFromItem[];
  key: string;
  label: string;
}

const renderOption = (item: StartFromItem): React.ReactNode => (
  <Option disabled={item.disabled} key={item.value} value={item.value}>
    {item.label}
  </Option>
);

const byStartTimeDesc = (a: CommandTask, b: CommandTask): number =>
  Date.parse(b.startTime) - Date.parse(a.startTime);

/**
 * The launch form's "Start from" picker. Shells and JupyterLabs share one config
 * format, so it offers both types, each item labelled with its type, whichever
 * type the form launches:
 * - Recent on cluster: the user's own shells and JupyterLabs that the master
 *   still knows (running, or ended in about the last 24 hours and not lost in a
 *   master restart), newest first. A config is fetched, from the API of the
 *   task's own type, only when the task is picked.
 * - Recently launched in this browser: utils/launchHistory, newest first.
 * - Templates.
 * The current item, whichever group it comes from, is listed alone in a
 * "Selected" group above them and left out of its own group. The dropdown
 * scrolls to the selected row when it opens; as the first row it keeps the list
 * at the top, with the recent tasks and the history right below, even when the
 * item is a template far down the list.
 */
const StartFromSelect: React.FC<Props> = ({
  allowedWorkspaceIds,
  autoSelect,
  defaultTemplate,
  id,
  initialTask,
  loadingTaskId,
  lockedWorkspaceId,
  onAutoSelected,
  onChange,
  onLoadingTaskChange,
  onResolve,
  value,
}: Props) => {
  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const userId = currentUser?.id;
  const [templates, setTemplates] = useState<Template[]>();
  const [recentTasks, setRecentTasks] = useState<CommandTask[]>([]);
  const [historyVersion, setHistoryVersion] = useState(0);
  const pendingTaskId = useRef<string>();

  const localEntries = useMemo(
    () => listAllLaunchHistory(userId),
    // historyVersion re-reads the history after it was cleared.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [userId, historyVersion],
  );

  const taskOptions = useMemo(() => {
    if (!initialTask || recentTasks.some((task) => task.id === initialTask.id)) return recentTasks;
    return [initialTask, ...recentTasks];
  }, [initialTask, recentTasks]);

  const isAllowed = useCallback(
    (workspaceId: number) =>
      lockedWorkspaceId !== undefined
        ? workspaceId === lockedWorkspaceId
        : allowedWorkspaceIds.includes(workspaceId),
    [allowedWorkspaceIds, lockedWorkspaceId],
  );

  useEffect(() => {
    let active = true;
    getTaskTemplates({})
      .then((list) => active && setTemplates(list))
      .catch((e) => {
        if (active) setTemplates([]);
        handleError(e);
      });
    return () => {
      active = false;
    };
  }, []);

  useEffect(() => {
    if (userId === undefined) return;
    let active = true;
    const params: GetShellsParams = {
      limit: RECENT_TASK_LIMIT,
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
      users: [String(userId)],
      workspaceId: lockedWorkspaceId,
    };
    // One type failing to load leaves the other listed.
    Promise.allSettled([getShells(params), getJupyterLabs(params)]).then((results) => {
      results.forEach((result) => {
        if (result.status === 'rejected') {
          handleError(result.reason, {
            publicSubject: 'Unable to fetch recent tasks.',
            silent: true,
            type: ErrorType.Api,
          });
        }
      });
      if (!active) return;
      const tasks = results
        .flatMap((result) => (result.status === 'fulfilled' ? result.value : []))
        .filter((task) => task.userId === userId)
        .sort(byStartTimeDesc)
        .slice(0, RECENT_TASK_LIMIT);
      setRecentTasks(tasks);
    });
    return () => {
      active = false;
    };
  }, [lockedWorkspaceId, userId]);

  const resolveTask = useCallback(
    async (task: CommandTask) => {
      pendingTaskId.current = task.id;
      onLoadingTaskChange(task.id);
      try {
        // The task's own type, not the type the form launches.
        const getConfig = task.type === CommandType.Shell ? getShellConfig : getJupyterLabConfig;
        const config = await getConfig({ commandId: task.id });
        if (pendingTaskId.current !== task.id) return;
        onResolve({ config, kind: 'config', redactedEnv: [], workspaceId: task.workspaceId });
      } catch (e) {
        if (pendingTaskId.current !== task.id) return;
        if (isNotFound(e as Error)) {
          setRecentTasks((tasks) => tasks.filter((item) => item.id !== task.id));
        }
        onChange?.(undefined);
        onResolve({ kind: 'blank' });
        handleError(e, {
          level: ErrorLevel.Warn,
          publicMessage:
            'The master keeps ended tasks for about 24 hours and forgets them when it restarts.',
          publicSubject: `Unable to load the config of ${task.name}.`,
          silent: false,
          type: ErrorType.Server,
        });
      } finally {
        if (pendingTaskId.current === task.id) {
          pendingTaskId.current = undefined;
          onLoadingTaskChange(undefined);
        }
      }
    },
    [onChange, onLoadingTaskChange, onResolve],
  );

  const resolve = useCallback(
    (next?: string) => {
      pendingTaskId.current = undefined;
      onLoadingTaskChange(undefined);
      if (!next) {
        onResolve({ kind: 'blank' });
      } else if (next.startsWith(TEMPLATE_PREFIX)) {
        const template = templates?.find((item) => item.name === templateNameFromStartFrom(next));
        if (template) onResolve({ kind: 'template', template });
      } else if (next.startsWith(LOCAL_PREFIX)) {
        const entry = localEntries.find((item) => startFromLocalValue(item.id) === next);
        if (entry) {
          onResolve({
            config: entry.config,
            kind: 'config',
            redactedEnv: entry.redactedEnv ?? [],
            workspaceId: entry.workspaceId,
          });
        }
      } else if (next.startsWith(TASK_PREFIX)) {
        const task = taskOptions.find((item) => startFromTaskValue(item.id) === next);
        if (task) resolveTask(task);
      }
    },
    [localEntries, onLoadingTaskChange, onResolve, resolveTask, taskOptions, templates],
  );

  const handleChange = useCallback(
    (selected: SelectValue) => {
      const next = typeof selected === 'string' && selected !== '' ? selected : undefined;
      onChange?.(next);
      resolve(next);
    },
    [onChange, resolve],
  );

  const groups = useMemo(
    (): StartFromGroup[] => [
      {
        items: taskOptions.map((task) => ({
          disabled: !isAllowed(task.workspaceId),
          label: taskLabel(task),
          value: startFromTaskValue(task.id),
        })),
        key: 'recent',
        label: 'Recent on cluster',
      },
      {
        items: localEntries.map((entry) => ({
          disabled: !isAllowed(entry.workspaceId),
          label: localLabel(entry),
          value: startFromLocalValue(entry.id),
        })),
        key: 'local',
        label: 'Recently launched in this browser',
      },
      {
        items: (templates ?? []).map((template) => ({
          label: template.name,
          value: startFromTemplateValue(template.name),
        })),
        key: 'templates',
        label: 'Templates',
      },
    ],
    [isAllowed, localEntries, taskOptions, templates],
  );

  const selectedItem = useMemo(
    () =>
      !value
        ? undefined
        : groups.flatMap((group) => group.items).find((item) => item.value === value),
    [groups, value],
  );

  const handleClearHistory = useCallback(() => {
    clearAllLaunchHistory(userId);
    setHistoryVersion((version) => version + 1);
    if (value?.startsWith(LOCAL_PREFIX)) handleChange(undefined);
  }, [handleChange, userId, value]);

  // "Launch Again": start from the given task right away.
  useEffect(() => {
    if (!autoSelect || !initialTask) return;
    onAutoSelected();
    onChange?.(startFromTaskValue(initialTask.id));
    resolveTask(initialTask);
  }, [autoSelect, initialTask, onAutoSelected, onChange, resolveTask]);

  // Otherwise preselect the template of the user's last launch once the
  // templates load, unless something was picked already. Only the picker is
  // set: the form already holds the user's last pool and slots, so the
  // template's resources are not copied in as they are on an explicit pick.
  useEffect(() => {
    if (!autoSelect || initialTask || !templates) return;
    onAutoSelected();
    if (value || !defaultTemplate) return;
    if (templates.some((item) => item.name === defaultTemplate)) {
      onChange?.(startFromTemplateValue(defaultTemplate));
    }
  }, [autoSelect, defaultTemplate, initialTask, onAutoSelected, onChange, templates, value]);

  return (
    <div data-test-component="start-from-select">
      <Select
        allowClear
        id={id}
        loading={loadingTaskId !== undefined}
        placeholder="Blank: cluster defaults (optional)"
        value={value}
        onChange={handleChange}>
        {selectedItem && (
          <OptGroup key="selected" label="Selected">
            {renderOption(selectedItem)}
          </OptGroup>
        )}
        {groups.map((group) => {
          const items = group.items.filter((item) => item !== selectedItem);
          return (
            items.length > 0 && (
              <OptGroup key={group.key} label={group.label}>
                {items.map(renderOption)}
              </OptGroup>
            )
          );
        })}
      </Select>
      {localEntries.length > 0 && (
        <Button size="small" type="text" onClick={handleClearHistory}>
          Clear browser history
        </Button>
      )}
    </div>
  );
};

export default StartFromSelect;
