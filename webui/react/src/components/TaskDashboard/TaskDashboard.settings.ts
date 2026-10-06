import { array, boolean, literal, number, string, undefined as undefinedType, union } from 'io-ts';

import { InteractiveTableSettings } from 'components/Table/InteractiveTable';
import { SettingsConfig } from 'hooks/useSettings';
import { ValueOf } from 'types';

import { DashboardScope, RunKind, SlotsFilter, StateGroup } from './runRows';

export type TaskDashboardColumnName =
  | 'action'
  | 'endTime'
  | 'id'
  | 'kind'
  | 'location'
  | 'name'
  | 'resourcePool'
  | 'slots'
  | 'startTime'
  | 'state'
  | 'user';

export const DEFAULT_COLUMNS: TaskDashboardColumnName[] = [
  'kind',
  'id',
  'name',
  'state',
  'user',
  'location',
  'resourcePool',
  'slots',
  'startTime',
  'endTime',
];

export const DEFAULT_COLUMN_WIDTHS: Record<TaskDashboardColumnName, number> = {
  action: 46,
  endTime: 117,
  id: 100,
  kind: 64,
  location: 180,
  name: 220,
  resourcePool: 128,
  slots: 72,
  startTime: 117,
  state: 120,
  user: 85,
};

/**
 * Columns stored before the Slots column existed, with Slots after Resource Pool (else last) and its
 * default width at the same place; undefined when Slots is already there. The table would otherwise
 * add it at the end.
 */
export const withSlotsColumn = (
  columns: TaskDashboardColumnName[],
  columnWidths: number[] = [],
): { columnWidths: number[]; columns: TaskDashboardColumnName[] } | undefined => {
  if (columns.includes('slots')) return undefined;
  const at = columns.includes('resourcePool')
    ? columns.indexOf('resourcePool') + 1
    : columns.length;
  const widths = [...columnWidths];
  if (widths.length >= at) widths.splice(at, 0, DEFAULT_COLUMN_WIDTHS.slots);
  return {
    columns: [...columns.slice(0, at), 'slots', ...columns.slice(at)],
    columnWidths: widths,
  };
};

/** The page size, by default and at most: each experiment row carries its whole config. */
export const DEFAULT_PAGE_SIZE = 20;
export const MAX_PAGE_SIZE = 100;

export const Owner = {
  All: 'all',
  Mine: 'mine',
} as const;

export type Owner = ValueOf<typeof Owner>;

export interface Settings extends InteractiveTableSettings {
  columns: TaskDashboardColumnName[];
  owner: Owner;
  search?: string;
  slots?: SlotsFilter;
  state?: StateGroup[];
  /** The kinds to list; none means all. The URL key and values are those of the old task list. */
  type?: RunKind[];
  /** On the global page, the runs of one workspace. */
  workspace?: number;
}

/** The settings a reset clears, and that the filter counter counts. */
export const FILTER_KEYS: Array<keyof Settings> = [
  'type',
  'state',
  'owner',
  'slots',
  'search',
  'workspace',
];

const scopeKey = (scope: DashboardScope): string => {
  switch (scope.type) {
    case 'global':
      return 'global';
    case 'workspace':
      return `ws-${scope.workspaceId}`;
    case 'project':
      return `project-${scope.projectId}`;
  }
};

/**
 * The settings of one dashboard, stored apart for each page (the Jobs pages and the tasks-only
 * view) and each scope (all workspaces, a workspace, a project).
 */
const settingsConfig = (scope: DashboardScope, experiments: boolean): SettingsConfig<Settings> => ({
  settings: {
    columns: {
      defaultValue: DEFAULT_COLUMNS,
      skipUrlEncoding: true,
      storageKey: 'columns',
      type: array(
        union([
          literal('action'),
          literal('endTime'),
          literal('id'),
          literal('kind'),
          literal('location'),
          literal('name'),
          literal('resourcePool'),
          literal('slots'),
          literal('startTime'),
          literal('state'),
          literal('user'),
        ]),
      ),
    },
    columnWidths: {
      defaultValue: DEFAULT_COLUMNS.map((col) => DEFAULT_COLUMN_WIDTHS[col]),
      skipUrlEncoding: true,
      storageKey: 'columnWidths',
      type: array(number),
    },
    owner: {
      defaultValue: Owner.All,
      storageKey: 'owner',
      type: union([literal(Owner.All), literal(Owner.Mine)]),
    },
    search: {
      defaultValue: undefined,
      storageKey: 'search',
      type: union([undefinedType, string]),
    },
    slots: {
      defaultValue: undefined,
      storageKey: 'slots',
      type: union([undefinedType, literal(SlotsFilter.Gpu), literal(SlotsFilter.CpuOnly)]),
    },
    sortDesc: {
      defaultValue: true,
      skipUrlEncoding: true,
      storageKey: 'sortDesc',
      type: boolean,
    },
    state: {
      defaultValue: undefined,
      storageKey: 'state',
      type: union([
        undefinedType,
        array(
          union([
            literal(StateGroup.Active),
            literal(StateGroup.Paused),
            literal(StateGroup.Ended),
          ]),
        ),
      ]),
    },
    tableLimit: {
      defaultValue: DEFAULT_PAGE_SIZE,
      storageKey: 'tableLimit',
      type: number,
    },
    tableOffset: {
      defaultValue: 0,
      storageKey: 'tableOffset',
      type: number,
    },
    type: {
      defaultValue: undefined,
      storageKey: 'type',
      type: union([
        undefinedType,
        array(
          union([
            literal(RunKind.Experiment),
            literal(RunKind.GenericTask),
            literal(RunKind.JupyterLab),
            literal(RunKind.Shell),
            literal(RunKind.Command),
            literal(RunKind.TensorBoard),
          ]),
        ),
      ]),
    },
    workspace: {
      defaultValue: undefined,
      storageKey: 'workspace',
      type: union([undefinedType, number]),
    },
  },
  storagePath: `${experiments ? 'jobs' : 'tasks'}-dashboard-${scopeKey(scope)}`,
});

export default settingsConfig;
