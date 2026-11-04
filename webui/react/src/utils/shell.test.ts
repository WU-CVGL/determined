import { launchShell as apiLaunchShell, previewShell as apiPreviewShell } from 'services/api';
import userStore from 'stores/users';
import { CommandState, CommandType } from 'types';
import { DetError } from 'utils/error';
import { listLaunchHistory } from 'utils/launchHistory';
import { processApiError } from 'utils/service';

import {
  actionableErrorMessage,
  launchShell,
  PREVIEW_ERROR,
  previewShell,
  shellRequest,
} from './shell';

vi.mock('services/api', () => ({ launchShell: vi.fn(), previewShell: vi.fn() }));

/** A refusal of the master as the services layer throws it. */
const masterError = (status: number, message: string) =>
  processApiError(
    'previewShell',
    new Response(JSON.stringify({ error: { code: 3, error: message } }), { status }),
  );

const launched = {
  command: {
    id: 'shell-1',
    name: 'Shell (lively-calm-fox)',
    resourcePool: 'default',
    startTime: '2026-01-01T00:00:00Z',
    state: CommandState.Queued,
    type: CommandType.Shell,
    userId: 1,
    workspaceId: 3,
  },
  config: { description: 'Shell (lively-calm-fox)', services: { jupyter: {} } },
  warnings: [],
};

describe('shell', () => {
  beforeEach(() => {
    window.localStorage.clear();
    vi.mocked(apiLaunchShell).mockReset().mockResolvedValue(launched);
    vi.mocked(apiPreviewShell)
      .mockReset()
      .mockResolvedValue({ services: { jupyter: {} } });
    userStore.updateCurrentUser({ id: 1, isActive: true, isAdmin: false, username: 'me' });
  });

  describe('shellRequest', () => {
    it('sends the simple fields with the template and the services', () => {
      expect(
        shellRequest({
          name: 'lab',
          pool: 'gpu',
          services: { jupyter: true, vscode: false },
          slots: 2,
          template: 'tpl',
          workspaceId: 3,
        }),
      ).toEqual({
        config: {
          description: 'lab',
          resources: { resource_pool: 'gpu', slots: 2 },
          services: { jupyter: {}, vscode: { enabled: false } },
        },
        templateName: 'tpl',
        workspaceId: 3,
      });
      expect(shellRequest({ template: '' }).templateName).toBeUndefined();
      // Without services, the master runs its defaults.
      expect(shellRequest({ template: 'tpl' }).config).not.toHaveProperty('services');
    });

    it('sends a full config as it is, without a template', () => {
      const config = { description: 'debug box', services: { vscode: {} } };
      expect(shellRequest({ config, services: { jupyter: true }, template: 'tpl' })).toEqual({
        config: { description: 'debug box', services: { vscode: {} } },
        templateName: undefined,
        workspaceId: undefined,
      });
    });
  });

  it('previews and launches the same request for the same options', async () => {
    const options = {
      name: 'lab',
      services: { jupyter: true, vscode: true },
      slots: 1,
      workspaceId: 3,
    };
    expect(await previewShell(options)).toEqual({ services: { jupyter: {} } });
    await launchShell(options);
    expect(vi.mocked(apiPreviewShell).mock.calls[0][0]).toEqual(shellRequest(options));
    expect(vi.mocked(apiLaunchShell).mock.calls[0][0]).toEqual(shellRequest(options));
  });

  it('records a launch in this browser’s history', async () => {
    await launchShell({ workspaceId: 3 });
    expect(listLaunchHistory(1)).toEqual([
      expect.objectContaining({ config: { services: { jupyter: {} } }, workspaceId: 3 }),
    ]);
  });

  describe('a refused preview', () => {
    it.each([400, 403])('is thrown with the master’s message for a %s', async (status) => {
      vi.mocked(apiPreviewShell).mockRejectedValue(await masterError(status, 'no such pool'));
      await expect(previewShell({})).rejects.toThrow('no such pool');
    });

    it.each([
      ['a server error', () => masterError(500, 'database down')],
      ['an unreachable master', () => Promise.resolve(new DetError(new TypeError('fetch')))],
      ['a plain error', () => Promise.resolve(new Error('boom'))],
    ])('is thrown with the generic message for %s', async (_, error) => {
      vi.mocked(apiPreviewShell).mockRejectedValue(await error());
      await expect(previewShell({})).rejects.toThrow(PREVIEW_ERROR);
    });
  });

  describe('actionableErrorMessage', () => {
    it('reads the message of an InvalidArgument or PermissionDenied refusal only', async () => {
      expect(actionableErrorMessage(await masterError(400, 'unknown service'))).toBe(
        'unknown service',
      );
      expect(actionableErrorMessage(await masterError(403, 'denied'))).toBe('denied');
      expect(actionableErrorMessage(await masterError(404, 'not found'))).toBeUndefined();
      expect(actionableErrorMessage(new Error('x'))).toBeUndefined();
      expect(actionableErrorMessage(undefined)).toBeUndefined();
    });
  });
});
