import { RawResourcePoolAccess } from 'services/decoder';

const GPU_A100_WARNINGS = [
  '"gpu-a100" is the cluster\'s default compute pool: submissions that omit ' +
    'resources.resource_pool are refused for users without a grant on "gpu-a100"',
  '"gpu-a100" is the default compute pool of workspace "vision": submissions there that omit ' +
    'resources.resource_pool are refused for users without a grant on "gpu-a100"',
];

const OLD_POOL_WARNING =
  'no resource pool named "old-pool" exists; the setting applies to a pool created with this name';

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
      warnings: [],
      warnings_if_restricted: [
        '"cpu" is the cluster\'s default aux pool: submissions that omit ' +
          'resources.resource_pool are refused for users without a grant on "cpu"',
      ],
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
      warnings: GPU_A100_WARNINGS,
      warnings_if_restricted: GPU_A100_WARNINGS,
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
      warnings: [],
      warnings_if_restricted: [],
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
      warnings: [OLD_POOL_WARNING],
      warnings_if_restricted: [OLD_POOL_WARNING],
      workspace_defaults: [],
    },
  ],
};
