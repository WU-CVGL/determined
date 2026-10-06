import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getProject } from 'services/api';
import { Project, WorkspaceState } from 'types';

import ProjectDetails from './ProjectDetails';

const flags = vi.hoisted(() => ({ flat_runs: true }));

vi.mock('services/api', () => ({
  getProject: vi.fn(),
  postUserActivity: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/useFeature', () => ({
  default: () => ({ isOn: (feature: string) => feature === 'flat_runs' && flags.flat_runs }),
}));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canViewWorkspace: () => true, loading: false }),
}));
vi.mock('components/ProjectActionDropdown', () => ({
  useProjectActionMenu: () => ({ contextHolders: null, menu: [], onClick: () => {} }),
}));
vi.mock('components/TaskDashboard/TaskDashboard', () => ({
  default: ({ projectId }: { projectId?: number }) => (
    <div data-testid="dashboard">{`project ${projectId}`}</div>
  ),
}));
vi.mock('pages/FlatRuns/FlatRuns', () => ({ default: () => <div>runs</div> }));
vi.mock('components/Searches/Searches', () => ({ default: () => <div>searches</div> }));
vi.mock('pages/ProjectNotes', () => ({ default: () => <div>notes</div> }));
vi.mock('pages/ExperimentList', () => ({ default: () => <div>experiments</div> }));
vi.mock('pages/F_ExpList/F_ExperimentList', () => ({ default: () => <div>experiments</div> }));

const project = (id: number): Project => ({
  archived: false,
  id,
  immutable: id === 1,
  name: id === 1 ? 'Uncategorized' : 'vision',
  notes: [],
  state: WorkspaceState.Unspecified,
  userId: 1,
  workspaceId: id === 1 ? 1 : 2,
  workspaceName: id === 1 ? 'Uncategorized' : 'lab',
});

const setup = (id: number, tab = 'jobs') => {
  vi.mocked(getProject).mockResolvedValue(project(id));
  return render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <MemoryRouter initialEntries={[`/projects/${id}/${tab}`]}>
            <Routes>
              <Route element={<ProjectDetails />} path="/projects/:projectId/:tab" />
            </Routes>
          </MemoryRouter>
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

const tabLabels = () => screen.getAllByRole('tab').map((tab) => tab.textContent);

describe('ProjectDetails', () => {
  afterEach(() => {
    flags.flat_runs = true;
    vi.clearAllMocks();
  });

  it("has a Jobs tab after Searches with the project's experiments and generic tasks", async () => {
    setup(5);
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('project 5');
    expect(tabLabels()).toEqual(['Runs', 'Searches', 'Jobs', 'Notes']);
  });

  it('has the Jobs tab in Uncategorized (project 1) too', async () => {
    setup(1);
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('project 1');
    expect(tabLabels()).toContain('Jobs');
  });

  it('puts the Jobs tab after Experiments without flat runs, also in Uncategorized', async () => {
    flags.flat_runs = false;
    setup(1);
    expect(await screen.findByTestId('dashboard')).toHaveTextContent('project 1');
    expect(tabLabels()).toEqual(['Experiments', 'Jobs']);
  });

  it('still opens on Runs', async () => {
    setup(5, 'runs');
    expect(await screen.findByText('runs')).toBeInTheDocument();
    expect(screen.queryByTestId('dashboard')).not.toBeInTheDocument();
  });
});
