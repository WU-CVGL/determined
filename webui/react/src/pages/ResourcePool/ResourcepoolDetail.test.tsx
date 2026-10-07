import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { Loaded } from 'hew/utils/loadable';
import { Map } from 'immutable';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { gpuTopologyCase } from 'fixtures/gpuTopologyCases';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import * as Api from 'services/api-ts-sdk';
import clusterStore from 'stores/cluster';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import { Agent, FullJob, JobState, JobType, ResourcePool, ResourceType } from 'types';

import ResourcepoolDetail from './ResourcepoolDetail';

const mocks = vi.hoisted(() => ({ agents: [] as unknown[] }));

const POOL = {
  auxContainerCapacityPerAgent: 0,
  details: {},
  maxAgents: 0,
  name: 'gpus',
  schedulerType: Api.V1SchedulerType.PRIORITY,
  slotsAvailable: 7,
  slotType: ResourceType.CUDA,
} as unknown as ResourcePool;

const JOB = {
  allocatedSlots: 2,
  entityId: '812',
  isPreemptible: true,
  jobId: 'job-1',
  name: 'sweep',
  placement: [{ agentId: 'node01', deviceIds: [0, 1] }],
  priority: 42,
  requestedSlots: 2,
  resourcePool: 'gpus',
  submissionTime: new Date('2026-01-01T00:00:00Z'),
  summary: { jobsAhead: 0, state: JobState.SCHEDULED },
  type: JobType.EXPERIMENT,
  userId: 7,
  username: 'alice',
  workspaceId: 1,
} as unknown as FullJob;

vi.mock('services/api', () => ({
  getAgents: vi.fn(() => Promise.resolve(mocks.agents)),
  getJobQ: vi.fn(() => Promise.resolve({ jobs: [JOB], pagination: { total: 1 } })),
  getJobQStats: vi.fn(() =>
    Promise.resolve({
      results: [
        {
          resourcePool: 'gpus',
          stats: { preemptibleCount: 0, queuedCount: 0, scheduledCount: 1 },
        },
      ],
    }),
  ),
  getResourcePools: vi.fn(() => Promise.resolve([POOL])),
  getTask: vi.fn(),
  getUsers: vi.fn(() => Promise.resolve({ pagination: {}, users: [] })),
  updateUserSetting: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/usePermissions', () => {
  const checks = {
    canCreateWorkspaceNSC: () => true,
    canManageResourcePoolBindings: false,
    canModifyExperiment: () => true,
    canModifyWorkspaceNSC: () => true,
    loading: false,
  };
  return { default: () => checks };
});
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => false }) }));
vi.mock('components/NtscLaunchModal', () => ({ default: () => null }));

const agentOf = (gpuTopology?: Api.V1GpuTopology): Agent => ({
  gpuTopology,
  id: 'node01',
  registeredTime: 0,
  resourcePools: ['gpus'],
  resources: [0, 1, 2, 3, 5, 6, 7].map((id) => ({
    enabled: true,
    id: String(id),
    name: 'NVIDIA GeForce RTX 3090',
    type: ResourceType.CUDA,
  })),
  slotStats: { brandStats: {}, typeStats: {} },
});

const setup = async (agent: Agent) => {
  mocks.agents = [agent];
  clusterStore.fetchAgents();
  clusterStore.fetchResourcePools();
  await waitFor(() => {
    expect(clusterStore.agents.get().isLoaded).toBe(true);
    expect(clusterStore.resourcePools.get().isLoaded).toBe(true);
  });
  return render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <DndProvider backend={HTML5Backend}>
            <SettingsProvider>
              <MemoryRouter initialEntries={['/resourcepool/gpus/active']}>
                <ConfirmationProvider>
                  <Routes>
                    <Route element={<ResourcepoolDetail />} path="/resourcepool/:poolname/:tab" />
                  </Routes>
                </ConfirmationProvider>
              </MemoryRouter>
            </SettingsProvider>
          </DndProvider>
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

const tile = (name: string) =>
  within(screen.getByRole('article', { name: 'GPU topology of agent node01' })).getByRole('group', {
    name: new RegExp(`^${name},`),
  });

const highlightedTiles = () => document.querySelectorAll('[data-highlighted="true"]');

describe('ResourcepoolDetail', () => {
  beforeEach(() => {
    userStore.reset();
    userStore.updateCurrentUser({ id: 7, isActive: true, isAdmin: false, username: 'a' });
    userSettings.reset();
    userSettings
      ._forUseSettingsOnly()
      .set(
        Loaded(
          Map({ [`job-queue-${JobState.SCHEDULED}`]: {}, [`job-queue-${JobState.QUEUED}`]: {} }),
        ),
      );
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  it("outlines a job's GPUs from the Active tab until the tab closes", async () => {
    await setup(agentOf(gpuTopologyCase('node01 with the exclude list')));
    const gpus = await screen.findByRole('button', { name: 'node01: 0, 1' });

    await userEvent.click(gpus);
    await waitFor(() => expect(tile('Slot 0')).toHaveAttribute('data-highlighted', 'true'));
    expect(tile('Slot 1')).toHaveAttribute('data-highlighted', 'true');
    expect(highlightedTiles()).toHaveLength(2);

    await userEvent.click(screen.getByRole('tab', { name: /Queued/ }));
    await waitFor(() => expect(highlightedTiles()).toHaveLength(0));
    expect(screen.queryByRole('button', { name: 'node01: 0, 1' })).toBeNull();
  });

  it('lists the GPUs as text when no agent shows GPU tiles', async () => {
    await setup(agentOf(undefined));
    await screen.findByText('node01: 0, 1');
    expect(screen.queryByRole('button', { name: 'node01: 0, 1' })).toBeNull();
    expect(screen.queryByRole('article', { name: 'GPU topology of agent node01' })).toBeNull();
  });
});
