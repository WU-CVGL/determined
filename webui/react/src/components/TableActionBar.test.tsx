import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { Loaded } from 'hew/utils/loadable';

import { experiment } from 'components/ExperimentActionDropdown.test.mock';
import { FilterFormStore } from 'components/FilterForm/components/FilterFormStore';
import { RowHeight } from 'components/OptionsMenu';
import { V1LocationType } from 'services/api-ts-sdk';
import { Project, RunState } from 'types';
import { isDangerMenuItem } from 'utils/tests/menu';

import TableActionBar from './TableActionBar';

const project: Project = {
  archived: false,
  description: '',
  id: 1,
  immutable: false,
  name: 'test',
  notes: [],
  numActiveExperiments: 1,
  numExperiments: 1,
  state: 'UNSPECIFIED',
  userId: 1,
  workspaceId: 1,
  workspaceName: '',
};

vi.mock('services/api', () => ({
  getExperiments: vi.fn(() =>
    Promise.resolve({ experiments: [{ ...experiment, state: RunState.Running }] }),
  ),
  getWorkspaces: vi.fn(() => Promise.resolve({ workspaces: [] })),
}));

vi.mock('hooks/useMobile', () => ({ default: () => false }));

describe('TableActionBar', () => {
  it('shows Kill and Delete in red in the Actions menu and the other actions not', async () => {
    const user = userEvent.setup();
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <TableActionBar
          columnGroups={[]}
          formStore={new FilterFormStore(V1LocationType.EXPERIMENT)}
          initialVisibleColumns={[]}
          isOpenFilter={false}
          labelPlural="searches"
          labelSingular="search"
          project={project}
          projectColumns={Loaded([])}
          rowHeight={RowHeight.MEDIUM}
          selectedExperimentIds={[experiment.id]}
          sorts={[]}
          total={Loaded(1)}
        />
      </UIProvider>,
    );
    await user.click(screen.getByText('Actions'));
    await screen.findByText('Kill');
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('Delete')).toBe(true);
    ['Move', 'Archive', 'Pause', 'Stop'].forEach((label) =>
      expect(isDangerMenuItem(label)).toBe(false),
    );
  });
});
