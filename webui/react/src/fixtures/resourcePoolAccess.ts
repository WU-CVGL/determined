import { RawResourcePoolAccess } from 'services/decoder';

/** A response of GET /api/v1/resource-pool-access, as the master sends it. */
export const resourcePoolAccessResponse: { resource_pools: RawResourcePoolAccess[] } = {
  resource_pools: [
    {
      default_aux: true,
      default_compute: false,
      exists: true,
      mode: 'public',
      pool_name: 'cpu',
      restricted_at: null,
      restricted_by: null,
      users: [],
      workspace_defaults: [],
    },
    {
      default_aux: false,
      default_compute: true,
      exists: true,
      mode: 'restricted',
      pool_name: 'gpu-a100',
      restricted_at: '2026-10-01T08:00:00Z',
      restricted_by: 'admin',
      users: [
        { active: true, admin: false, id: 7, username: 'alice' },
        { active: false, admin: false, id: 9, username: 'carol' },
      ],
      workspace_defaults: [{ kind: 'compute', workspace: 'vision', workspace_id: 4 }],
    },
    {
      default_aux: false,
      default_compute: false,
      exists: true,
      mode: 'public',
      pool_name: 'gpu-h100',
      restricted_at: null,
      restricted_by: null,
      users: [{ active: true, admin: false, id: 8, username: 'bob' }],
      workspace_defaults: [],
    },
    {
      default_aux: false,
      default_compute: false,
      exists: false,
      mode: 'restricted',
      pool_name: 'old-pool',
      restricted_at: '2026-09-01T08:00:00Z',
      restricted_by: null,
      users: [],
      workspace_defaults: [],
    },
  ],
};
