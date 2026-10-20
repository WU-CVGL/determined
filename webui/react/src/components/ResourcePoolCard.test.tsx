import { render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { Suspense } from 'react';

import resourcePools from 'fixtures/responses/cluster/resource-pools.json';
import { ResourcePool } from 'types';

import ResourcePoolCard, { RenderAllocationBarResourcePool } from './ResourcePoolCard';

const rps = resourcePools as unknown as ResourcePool[];

vi.mock('services/api', () => ({
  getAgents: () => Promise.resolve([]),
  getResourcePoolBindings: () => Promise.resolve({ workspaceIds: [] }),
  getResourcePools: () => Promise.resolve({}),
}));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => false }) }));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canManageResourcePoolBindings: false }),
}));

const setup = (pool: ResourcePool) => {
  const view = render(
    <UIProvider theme={DefaultTheme.Light}>
      <Suspense>
        <RenderAllocationBarResourcePool resourcePool={pool} />
      </Suspense>
    </UIProvider>,
  );
  return { view };
};

describe('AllocationBarResourcePool', () => {
  it('displays resource pool slot allocation bar ', async () => {
    await rps.forEach(async (pool) => {
      const { view } = setup(pool);
      await waitFor(() => expect(screen.getByText('Allocated')).toBeInTheDocument());
      expect(view.getAllByText('Allocated', { exact: false }).length).toBeGreaterThan(0);
    });
  });
});

describe('ResourcePoolCard', () => {
  const card = (pool: ResourcePool) =>
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <ResourcePoolCard resourcePool={pool} />
      </UIProvider>,
    );

  it('stripes and tags a pool that the user may not use', async () => {
    card({ ...rps[0], usable: false });
    const name = await screen.findByText(rps[0].name);
    expect(name.closest('.base')).toHaveClass('striped');
    expect(screen.getByText('Restricted')).toBeInTheDocument();
  });

  it('shows a usable pool as before', async () => {
    card({ ...rps[0], usable: true });
    const name = await screen.findByText(rps[0].name);
    expect(name.closest('.base')).not.toHaveClass('striped');
    expect(screen.queryByText('Restricted')).toBeNull();
  });
});
