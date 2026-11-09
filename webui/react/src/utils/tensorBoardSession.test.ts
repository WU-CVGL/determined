import { makeToast } from 'hew/Toast';

import { getCurrentUser, getShells, launchShell } from 'services/api';
import userStore from 'stores/users';
import { CommandResponse, CommandState, CommandTask, CommandType } from 'types';
import { recordLaunch } from 'utils/launchHistory';
import { processApiError } from 'utils/service';

import {
  canonicalTensorBoardIds,
  handleTensorBoardError,
  openOrCreateTensorBoard,
  openTensorBoardSession,
  sameTensorBoardSources,
  tensorBoardSessionConfig,
} from './tensorBoardSession';

vi.mock('services/api', () => ({
  getCurrentUser: vi.fn(),
  getShells: vi.fn(),
  launchShell: vi.fn(),
}));

vi.mock('utils/launchHistory', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/launchHistory')>()),
  recordLaunch: vi.fn(),
}));

vi.mock('hew/Toast', async (importOriginal) => ({
  ...(await importOriginal<typeof import('hew/Toast')>()),
  makeToast: vi.fn(),
}));

const ME = { id: 7, isActive: true, isAdmin: false, username: 'me' };
const WORKSPACE = 3;

const shell = (overrides: Partial<CommandTask> = {}): CommandTask => ({
  id: 'shell-1',
  misc: { experimentIds: [1, 3], trialIds: [] },
  name: 'TensorBoard (brave-cat)',
  resourcePool: 'aux',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: ME.id,
  workspaceId: WORKSPACE,
  ...overrides,
});

const created: CommandResponse = {
  command: shell({ id: 'shell-new', state: CommandState.Queued }),
  config: {},
  warnings: [],
};

describe('tensorBoardSession', () => {
  beforeEach(() => {
    vi.mocked(getCurrentUser).mockReset().mockResolvedValue(ME);
    vi.mocked(getShells).mockReset().mockResolvedValue([]);
    vi.mocked(launchShell).mockReset().mockResolvedValue(created);
    vi.mocked(recordLaunch).mockReset();
    vi.mocked(makeToast).mockReset();
    userStore.updateCurrentUser(ME);
  });

  describe('canonicalTensorBoardIds', () => {
    it('sorts numerically and drops duplicates without changing the input', () => {
      const ids = [10, 9, 10, 2];
      expect(canonicalTensorBoardIds(ids)).toEqual([2, 9, 10]);
      expect(ids).toEqual([10, 9, 10, 2]);
      expect(canonicalTensorBoardIds()).toEqual([]);
    });
  });

  describe('tensorBoardSessionConfig', () => {
    it('sends only the non-empty lists, canonical', () => {
      expect(tensorBoardSessionConfig({ experimentIds: [3, 1, 1], trialIds: [] })).toEqual({
        idle_timeout: '5m',
        resources: { slots: 0 },
        services: { ssh: { enabled: false }, tensorboard: { experiment_ids: [1, 3] } },
      });
      expect(tensorBoardSessionConfig({ trialIds: [12, 4] })).toEqual({
        idle_timeout: '5m',
        resources: { slots: 0 },
        services: { ssh: { enabled: false }, tensorboard: { trial_ids: [4, 12] } },
      });
    });
  });

  describe('sameTensorBoardSources', () => {
    it('compares both lists', () => {
      expect(sameTensorBoardSources(shell(), [1, 3], [])).toBe(true);
      expect(
        sameTensorBoardSources(
          shell({ misc: { experimentIds: [3, 1], trialIds: [] } }),
          [1, 3],
          [],
        ),
      ).toBe(true);
      expect(sameTensorBoardSources(shell(), [1], [])).toBe(false);
      expect(sameTensorBoardSources(shell(), [1, 3, 5], [])).toBe(false);
      expect(sameTensorBoardSources(shell(), [1, 3], [8])).toBe(false);
      expect(sameTensorBoardSources(shell({ misc: undefined }), [1, 3], [])).toBe(false);
    });
  });

  describe('openOrCreateTensorBoard', () => {
    it('creates a session with exactly the session config', async () => {
      const response = await openOrCreateTensorBoard({
        experimentIds: [3, 1, 1],
        workspaceId: WORKSPACE,
      });
      expect(response).toBe(created);
      expect(launchShell).toHaveBeenCalledTimes(1);
      expect(vi.mocked(launchShell).mock.calls[0][0]).toStrictEqual({
        config: {
          idle_timeout: '5m',
          resources: { slots: 0 },
          services: { ssh: { enabled: false }, tensorboard: { experiment_ids: [1, 3] } },
        },
        workspaceId: WORKSPACE,
      });
      expect(recordLaunch).not.toHaveBeenCalled();
    });

    it('sends only trial IDs for a trial selection', async () => {
      await openOrCreateTensorBoard({ trialIds: [5], workspaceId: WORKSPACE });
      expect(vi.mocked(launchShell).mock.calls[0][0].config).toEqual({
        idle_timeout: '5m',
        resources: { slots: 0 },
        services: { ssh: { enabled: false }, tensorboard: { trial_ids: [5] } },
      });
    });

    it("lists the viewer's own shells of the workspace, newest first", async () => {
      await openOrCreateTensorBoard({ experimentIds: [1], workspaceId: WORKSPACE });
      expect(getShells).toHaveBeenCalledWith({
        orderBy: 'ORDER_BY_DESC',
        sortBy: 'SORT_BY_START_TIME',
        users: [String(ME.id)],
        workspaceId: WORKSPACE,
      });
      expect(getCurrentUser).not.toHaveBeenCalled();
    });

    it('reads the current user when the store has none', async () => {
      userStore.reset();
      await openOrCreateTensorBoard({ experimentIds: [1], workspaceId: WORKSPACE });
      expect(getCurrentUser).toHaveBeenCalled();
      expect(vi.mocked(getShells).mock.calls[0][0].users).toEqual([String(ME.id)]);
    });

    it.each([CommandState.Running, CommandState.Queued])(
      "reuses the viewer's own %s session of the same sources",
      async (state) => {
        const match = shell({ id: 'shell-old', state });
        vi.mocked(getShells).mockResolvedValue([match]);
        const response = await openOrCreateTensorBoard({
          experimentIds: [3, 1],
          workspaceId: WORKSPACE,
        });
        expect(response).toEqual({ command: match, warnings: [] });
        expect(launchShell).not.toHaveBeenCalled();
      },
    );

    it.each([
      ['a terminating session', shell({ state: CommandState.Terminating })],
      ['a terminated session', shell({ state: CommandState.Terminated })],
      ["another user's session", shell({ userId: 8 })],
      ['a session in another workspace', shell({ workspaceId: 4 })],
      ['a superset', shell({ misc: { experimentIds: [1, 3, 5], trialIds: [] } })],
      ['a subset', shell({ misc: { experimentIds: [1], trialIds: [] } })],
      [
        'the same experiments with trials',
        shell({ misc: { experimentIds: [1, 3], trialIds: [9] } }),
      ],
      ['a shell without sources', shell({ misc: undefined })],
    ])('does not reuse %s', async (_, candidate) => {
      vi.mocked(getShells).mockResolvedValue([candidate]);
      const response = await openOrCreateTensorBoard({
        experimentIds: [1, 3],
        workspaceId: WORKSPACE,
      });
      expect(response).toBe(created);
      expect(launchShell).toHaveBeenCalledTimes(1);
    });

    it('creates without a reuse read when there is no workspace', async () => {
      await openOrCreateTensorBoard({ experimentIds: [1] });
      expect(getShells).not.toHaveBeenCalled();
      expect(getCurrentUser).not.toHaveBeenCalled();
      expect(vi.mocked(launchShell).mock.calls[0][0].workspaceId).toBeUndefined();
    });

    it('does not reuse a shell without sources for an empty selection', async () => {
      vi.mocked(getShells).mockResolvedValue([shell({ misc: undefined })]);
      await openOrCreateTensorBoard({ experimentIds: [], workspaceId: WORKSPACE });
      expect(getShells).not.toHaveBeenCalled();
      expect(launchShell).toHaveBeenCalledTimes(1);
    });

    it('creates nothing when the reuse read fails', async () => {
      vi.mocked(getShells).mockRejectedValue(new Error('unavailable'));
      await expect(
        openOrCreateTensorBoard({ experimentIds: [1], workspaceId: WORKSPACE }),
      ).rejects.toThrow('unavailable');
      expect(launchShell).not.toHaveBeenCalled();
    });

    it('creates nothing when the current user cannot be read', async () => {
      userStore.reset();
      vi.mocked(getCurrentUser).mockRejectedValue(new Error('unavailable'));
      await expect(
        openOrCreateTensorBoard({ experimentIds: [1], workspaceId: WORKSPACE }),
      ).rejects.toThrow('unavailable');
      expect(launchShell).not.toHaveBeenCalled();
    });
  });

  describe('openTensorBoardSession', () => {
    beforeEach(() => {
      vi.spyOn(global, 'open').mockReset();
    });

    afterEach(() => {
      vi.mocked(global.open).mockRestore();
    });

    it("opens a reused session's TensorBoard tab", async () => {
      vi.mocked(getShells).mockResolvedValue([shell({ id: 'shell-old' })]);
      await openTensorBoardSession({ experimentIds: [1, 3], workspaceId: WORKSPACE });
      expect(global.open).toHaveBeenCalledWith(
        expect.stringMatching(/\/tasks\/shell-old\/interactive\?tab=tensorboard$/),
        'shell-old',
        '',
      );
    });

    it("opens a new session's TensorBoard tab", async () => {
      await openTensorBoardSession({ experimentIds: [1], workspaceId: WORKSPACE });
      expect(global.open).toHaveBeenCalledWith(
        expect.stringMatching(/\/tasks\/shell-new\/interactive\?tab=tensorboard$/),
        'shell-new',
        '',
      );
    });

    it('opens nothing when the create is refused', async () => {
      vi.mocked(launchShell).mockRejectedValue(new Error('refused'));
      await expect(openTensorBoardSession({ experimentIds: [1] })).rejects.toThrow('refused');
      expect(global.open).not.toHaveBeenCalled();
    });
  });

  describe('handleTensorBoardError', () => {
    it("shows the master's text under the subject", async () => {
      const e = await processApiError(
        'launchShell',
        new Response(JSON.stringify({ error: { code: 5, error: "experiment '12' not found" } }), {
          status: 404,
        }),
      );
      handleTensorBoardError(e, 'Unable to View TensorBoard');
      expect(makeToast).toHaveBeenCalledWith({
        description: "experiment '12' not found",
        severity: 'Error',
        title: 'Unable to View TensorBoard',
      });
    });
  });
});
