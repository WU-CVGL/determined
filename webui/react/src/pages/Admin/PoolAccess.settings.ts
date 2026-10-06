import { array, boolean, number, string, undefined as undefinedType, union } from 'io-ts';

import { InteractiveTableSettings } from 'components/Table/InteractiveTable';
import { MINIMUM_PAGE_SIZE } from 'components/Table/Table';
import { SettingsConfig } from 'hooks/useSettings';

export type PoolAccessColumnName = 'poolName' | 'mode' | 'users' | 'workspaceDefaults' | 'warnings';

export const DEFAULT_COLUMNS: PoolAccessColumnName[] = [
  'poolName',
  'mode',
  'users',
  'workspaceDefaults',
  'warnings',
];

export const DEFAULT_COLUMN_WIDTHS: Record<PoolAccessColumnName, number> = {
  mode: 90,
  poolName: 200,
  users: 140,
  warnings: 100,
  workspaceDefaults: 200,
};

const config: SettingsConfig<InteractiveTableSettings> = {
  settings: {
    columns: {
      defaultValue: DEFAULT_COLUMNS,
      storageKey: 'columns',
      type: array(string),
    },
    columnWidths: {
      defaultValue: DEFAULT_COLUMNS.map((col: PoolAccessColumnName) => DEFAULT_COLUMN_WIDTHS[col]),
      skipUrlEncoding: true,
      storageKey: 'columnWidths',
      type: array(number),
    },
    row: {
      defaultValue: undefined,
      skipUrlEncoding: true,
      storageKey: 'row',
      type: union([undefinedType, union([array(string), array(number)])]),
    },
    sortDesc: {
      defaultValue: false,
      storageKey: 'sortDesc',
      type: boolean,
    },
    tableLimit: {
      defaultValue: MINIMUM_PAGE_SIZE,
      storageKey: 'tableLimit',
      type: number,
    },
    tableOffset: {
      defaultValue: 0,
      storageKey: 'tableOffset',
      type: number,
    },
  },
  storagePath: 'pool-access',
};

export default config;
