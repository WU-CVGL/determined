import { waitFor } from '@testing-library/react';
import { Loadable } from 'hew/utils/loadable';

import clusterStore from './cluster';

vi.mock('services/api', () => ({
  getResourcePools: vi.fn(() =>
    Promise.resolve([
      { name: 'c-restricted', usable: false },
      { name: 'b-open', usable: true },
      { name: 'a-restricted', usable: false },
      { name: 'd-open' },
    ]),
  ),
}));

vi.mock('routes/utils', () => ({
  serverAddress: () => 'http://localhost',
}));

describe('clusterStore.resourcePools', () => {
  it('lists pools by name, the ones the user may not use last', async () => {
    clusterStore.fetchResourcePools();
    await waitFor(() => {
      expect(Loadable.getOrElse([], clusterStore.resourcePools.get()).map((p) => p.name)).toEqual([
        'b-open',
        'd-open',
        'a-restricted',
        'c-restricted',
      ]);
    });
  });
});
