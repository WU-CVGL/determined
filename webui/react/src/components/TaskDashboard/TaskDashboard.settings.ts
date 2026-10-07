import { array, boolean, literal, number, string, undefined as undefinedType, union } from 'io-ts';
import _ from 'lodash';

import { ColumnLayout as Layout, withColumn } from 'components/Table/columnLayout';
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
  location: 230,
  name: 240,
  resourcePool: 128,
  slots: 72,
  startTime: 117,
  state: 120,
  user: 85,
};

/** The narrowest a column can be resized to, below its default width. */
export const MIN_COLUMN_WIDTH = 60;

/**
 * The widths of the default columns, which the settings give when none were stored: a new array
 * each time, as the table changes the widths it was given in place while a column is resized.
 */
export const defaultWidths = (): number[] =>
  DEFAULT_COLUMNS.map((col) => DEFAULT_COLUMN_WIDTHS[col]);

export type ColumnLayout = Layout<TaskDashboardColumnName>;

/**
 * The stored columns of a dashboard with one width for each, as the table binds them by place, or
 * undefined when they already are. It reads them as the settings give them, with the default for
 * either one that was never stored:
 * - Columns stored before the Slots column get it after Resource Pool (else last) at its default
 *   width, and each keeps its own width. The table would add Slots at the end.
 * - Widths stored without columns, as a resize stores them, are those of the default columns before
 *   Slots; the settings give the default columns of now, Slots included, so Slots gets its default
 *   width at its place.
 * - The default widths are those of the default columns: columns in another order get their own.
 * - A missing width is its column's default; widths past the last column are dropped.
 */
export const normalizedLayout = ({
  columns,
  columnWidths,
}: ColumnLayout): ColumnLayout | undefined => {
  const cols = columns.length > 0 ? columns : DEFAULT_COLUMNS;
  const widths = _.isEqual(columnWidths, defaultWidths())
    ? cols.map((col) => DEFAULT_COLUMN_WIDTHS[col])
    : columnWidths;
  const layout = withColumn(
    { columns: cols, columnWidths: widths },
    'slots',
    'resourcePool',
    DEFAULT_COLUMN_WIDTHS,
  );
  if (_.isEqual(layout.columns, columns) && _.isEqual(layout.columnWidths, columnWidths)) {
    return undefined;
  }
  return layout;
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
      defaultValue: defaultWidths(),
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
