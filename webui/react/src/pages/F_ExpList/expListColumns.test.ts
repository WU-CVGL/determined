import { DefaultTheme } from 'hew/Theme';
import { NotLoaded } from 'hew/utils/loadable';

import { BulkExperimentItem, RunState } from 'types';

import { getColumnDefs } from './expListColumns';

const experiment = (overrides: Partial<BulkExperimentItem>): BulkExperimentItem => ({
  archived: false,
  hyperparameters: {},
  id: 12,
  jobId: 'job-12',
  labels: [],
  name: 'mnist',
  numTrials: 1,
  projectId: 1,
  resourcePool: 'default',
  searcherType: 'single',
  startTime: '2026-01-01T00:00:00Z',
  state: RunState.Active,
  userId: 1,
  ...overrides,
});

const nameCell = (item: BulkExperimentItem) =>
  getColumnDefs({
    appTheme: DefaultTheme.Light,
    columnWidths: {},
    themeIsDark: false,
    users: NotLoaded,
  }).name.renderer({ experiment: item }, 0) as unknown as {
    copyData: string;
    data: { link: { title: string } };
  };

describe('experiment list Name column', () => {
  it('names an experiment whose title is not shared by its kind and ID', () => {
    const cell = nameCell(
      experiment({
        capabilities: {
          manageSharing: false,
          viewConfig: false,
          viewLogs: false,
          viewTitle: false,
        },
        name: undefined,
      }),
    );
    expect(cell.copyData).toBe('Experiment 12');
    expect(cell.data.link.title).toBe('Experiment 12');
  });

  it('names a shared experiment by its title', () => {
    expect(nameCell(experiment({})).copyData).toBe('mnist');
  });
});
