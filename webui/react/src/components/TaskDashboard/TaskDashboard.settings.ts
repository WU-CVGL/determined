import { array, boolean, literal, number, string, undefined as undefinedType, union } from 'io-ts';
import _ from 'lodash';

import { ColumnLayout as Layout, withColumn } from 'components/Table/columnLayout';
import { InteractiveTableSettings } from 'components/Table/InteractiveTable';
import { SettingsConfig } from 'hooks/useSettings';
import { ValueOf } from 'types';

import {
  DashboardScope,
  RUN_KINDS,
  RunKind,
  slotsQuery,
  SORT_KEYS,
  SortKey,
  STATE_GROUPS,
  StateGroup,
} from './runRows';

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
  kind: 84,
  location: 230,
  name: 240,
  resourcePool: 130,
  slots: 90,
  startTime: 117,
  state: 120,
  user: 115,
};

/** The narrowest a column can be resized to, below its default width. */
export const MIN_COLUMN_WIDTH = 60;

/**
 * The narrowest that a column with a sorter or a filter can be, to fit its title, its sort arrows
 * and its funnel. Stored widths below it show at it.
 */
export const MIN_SORT_FILTER_WIDTHS: Partial<Record<TaskDashboardColumnName, number>> = {
  endTime: 95,
  kind: 84,
  location: 190,
  name: 80,
  resourcePool: 130,
  slots: 90,
  startTime: 95,
  state: 105,
  user: 115,
};

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

/** "Mine" and everyone's, which 0.41.0 saved as `owner`. */
const LegacyOwner = {
  All: 'all',
  Mine: 'mine',
} as const;

export interface Settings extends InteractiveTableSettings {
  columns: TaskDashboardColumnName[];
  /** Saved by 0.41.0: "mine" turns into the user's own ID in `user` once, and is then cleared. */
  owner?: ValueOf<typeof LegacyOwner>;
  search?: string;
  /** Slot counts ('0', '1', ...) and Multi-node with its N ('multi:8'). */
  slots?: string[];
  sortDesc: boolean;
  sortKey: SortKey;
  state?: StateGroup[];
  /** The kinds to list; none means all. The URL key and values are those of the old task list. */
  type?: RunKind[];
  /** The owners' user IDs. */
  user?: number[];
  /** On the global page, the runs of these workspaces. */
  workspace?: number[];
}

/** The filters, which Clear Filters clears and counts. */
export const FILTER_KEYS = ['type', 'state', 'user', 'slots', 'search', 'workspace'] as const;

export type Filters = Pick<Settings, (typeof FILTER_KEYS)[number]>;

/** The filters cleared, also of the values that 0.41.0 saved. */
export const NO_FILTERS: Partial<Settings> = {
  owner: undefined,
  search: undefined,
  slots: undefined,
  state: undefined,
  type: undefined,
  user: undefined,
  workspace: undefined,
};

const isCount = (value: number) => Number.isInteger(value) && value >= 0;

const listOf = <T>(value: unknown, valid: (item: unknown) => item is T): T[] | undefined => {
  const items = (Array.isArray(value) ? value : value === undefined ? [] : [value]).filter(valid);
  return items.length > 0 ? [...new Set(items)] : undefined;
};

/**
 * Slot filters as the Jobs page saves them. GPU and CPU-only, which 0.41.0 saved, are more than 0
 * slots (Multi-node with N = 0) and 0 slots.
 */
const cleanSlots = (value: unknown): string[] | undefined =>
  listOf(
    (Array.isArray(value) ? value : value === undefined ? [] : [value]).map((item) =>
      item === 'gpu' ? 'multi:0' : item === 'cpu-only' ? '0' : item,
    ),
    (item): item is string => typeof item === 'string' && slotsQuery([item]) !== undefined,
  );

const cleanIds = (value: unknown): number[] | undefined =>
  listOf(value, (item): item is number => typeof item === 'number' && isCount(item));

export interface ReadFilters {
  /** The settings' update that saves the filters cleaned of what 0.41.0 saved, once. */
  cleanup?: Partial<Settings>;
  filters: Filters;
  /** "Mine" was saved, and waits for the signed-in user. */
  waitsForUser: boolean;
}

/**
 * The filters of the settings, cleaned of the values that 0.41.0 saved: GPU or CPU-only slots, one
 * workspace, and the owner, whose "Mine" becomes the user's own ID once the user is known.
 */
export const readFilters = (settings: Settings, currentUserId?: number): ReadFilters => {
  const raw = settings as unknown as Record<string, unknown>;
  const mine = raw.owner === LegacyOwner.Mine;
  const filters: Filters = {
    search: typeof raw.search === 'string' && raw.search ? raw.search : undefined,
    slots: cleanSlots(raw.slots),
    state: listOf(raw.state, (item): item is StateGroup =>
      (STATE_GROUPS as unknown[]).includes(item),
    ),
    type: listOf(raw.type, (item): item is RunKind => (RUN_KINDS as unknown[]).includes(item)),
    user: cleanIds(raw.user),
    workspace: cleanIds(raw.workspace),
  };
  if (mine && currentUserId !== undefined) filters.user = [currentUserId];
  const cleanup: Partial<Settings> = {};
  if (raw.owner !== undefined && (!mine || currentUserId !== undefined)) {
    cleanup.owner = undefined;
    if (mine) cleanup.user = filters.user;
  }
  if (!_.isEqual(filters.slots, raw.slots)) cleanup.slots = filters.slots;
  if (!_.isEqual(filters.workspace, raw.workspace)) cleanup.workspace = filters.workspace;
  return {
    cleanup: Object.keys(cleanup).length > 0 ? cleanup : undefined,
    filters,
    waitsForUser: mine && currentUserId === undefined,
  };
};

/** The settings that a URL sets: the filters, the sort and the page. */
const VIEW_KEYS = [...FILTER_KEYS, 'sortKey', 'sortDesc', 'tableOffset', 'tableLimit'];

const count = (value: string | null): number | undefined =>
  value !== null && /^\d+$/.test(value) ? Number(value) : undefined;

/**
 * The view that a URL sets, or undefined for a URL without any of its keys, which opens the saved
 * view. A URL with any of them sets all of them: a missing filter is no filter, a missing sort or
 * page the default one. Old /tasks links keep their kinds, search, owners, workspaces and sorts.
 */
export const urlView = (search: string, currentUserId?: number): Partial<Settings> | undefined => {
  const params = new URLSearchParams(search);
  // 0.41.0's "Mine".
  const mine = params.get('owner') === LegacyOwner.Mine;
  if (!VIEW_KEYS.some((key) => params.has(key)) && !mine) return undefined;
  const counts = (key: string) =>
    cleanIds(params.getAll(key).map((value) => count(value) ?? Number.NaN));
  const sortKey = params.get('sortKey');
  // An old sort, such as the old task list's by ID or by workspace, falls back to the default.
  const knownSort = sortKey === null || (SORT_KEYS as string[]).includes(sortKey);
  return {
    ...NO_FILTERS,
    search: params.get('search') || undefined,
    slots: cleanSlots(params.getAll('slots')),
    sortDesc: knownSort && params.has('sortDesc') ? params.get('sortDesc') === 'true' : true,
    sortKey: knownSort && sortKey !== null ? (sortKey as SortKey) : SortKey.StartTime,
    state: listOf(params.getAll('state'), (item): item is StateGroup =>
      (STATE_GROUPS as unknown[]).includes(item),
    ),
    tableLimit: count(params.get('tableLimit')) || DEFAULT_PAGE_SIZE,
    tableOffset: count(params.get('tableOffset')) ?? 0,
    type: listOf(params.getAll('type'), (item): item is RunKind =>
      (RUN_KINDS as unknown[]).includes(item),
    ),
    user: mine && currentUserId !== undefined ? [currentUserId] : counts('user'),
    workspace: counts('workspace'),
  };
};

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
      defaultValue: undefined,
      skipUrlEncoding: true,
      storageKey: 'owner',
      type: union([undefinedType, literal(LegacyOwner.All), literal(LegacyOwner.Mine)]),
    },
    search: {
      defaultValue: undefined,
      storageKey: 'search',
      type: union([undefinedType, string]),
    },
    slots: {
      defaultValue: undefined,
      storageKey: 'slots',
      type: union([undefinedType, array(string)]),
    },
    sortDesc: {
      defaultValue: true,
      storageKey: 'sortDesc',
      type: boolean,
    },
    sortKey: {
      defaultValue: SortKey.StartTime,
      storageKey: 'sortKey',
      type: union([
        literal(SortKey.EndTime),
        literal(SortKey.Kind),
        literal(SortKey.Name),
        literal(SortKey.ResourcePool),
        literal(SortKey.Slots),
        literal(SortKey.StartTime),
        literal(SortKey.State),
        literal(SortKey.User),
      ]),
      // The URL of the default view sets it too, as one with any filter, sort or page key does.
      urlFallback: true,
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
    user: {
      defaultValue: undefined,
      storageKey: 'user',
      type: union([undefinedType, array(number)]),
    },
    workspace: {
      defaultValue: undefined,
      storageKey: 'workspace',
      type: union([undefinedType, array(number)]),
    },
  },
  storagePath: `${experiments ? 'jobs' : 'tasks'}-dashboard-${scopeKey(scope)}`,
});

export default settingsConfig;
