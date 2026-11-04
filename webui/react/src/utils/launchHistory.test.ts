import {
  clearLaunchHistory,
  LAUNCH_HISTORY_LIMIT,
  listLaunchHistory,
  recordLaunch,
  removeLaunchHistoryEntry,
} from './launchHistory';

const launchConfig = (name: string) => ({
  description: name,
  entrypoint: ['/run/determined/ssh/shell-entrypoint.sh', '-p', '3333'],
  environment: {
    environment_variables: ['WANDB_API_KEY=secret-value', 'LANG=C.UTF-8'],
    image: { cpu: 'img' },
    registry_auth: { password: 'hunter2', username: 'me' },
  },
  resources: { priority: 42, resource_pool: 'default', slots: 1 },
});

describe('launchHistory', () => {
  beforeEach(() => {
    window.localStorage.clear();
    vi.restoreAllMocks();
  });

  it('records sanitized configs per user, newest first', () => {
    recordLaunch(1, { config: launchConfig('first'), workspaceId: 5 });
    recordLaunch(1, { config: launchConfig('second'), workspaceId: 5 });

    const entries = listLaunchHistory(1);
    expect(entries.map((entry) => entry.config.description)).toEqual(['second', 'first']);
    expect(entries[0].workspaceId).toBe(5);
    expect(entries[0].config).not.toHaveProperty('entrypoint');
    expect(entries[0].config.environment).not.toHaveProperty('registry_auth');
    expect(entries[0].config.resources).toEqual({ resource_pool: 'default', slots: 1 });

    expect(listLaunchHistory(2)).toEqual([]);
    expect(listLaunchHistory(undefined)).toEqual([]);
  });

  it('keys the stored data by user id', () => {
    recordLaunch(7, { config: launchConfig('a'), workspaceId: 1 });
    expect(Object.keys(window.localStorage)).toEqual(['u:7/launch-history/shell']);
  });

  it('leaves out credential-like environment variables and never stores secrets', () => {
    recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 });
    const [entry] = listLaunchHistory(1);
    expect(entry.redactedEnv).toEqual(['WANDB_API_KEY']);
    expect(entry.config.environment.environment_variables).toEqual(['LANG=C.UTF-8']);

    const raw = window.localStorage.getItem('u:1/launch-history/shell') ?? '';
    expect(raw).not.toContain('secret-value');
    expect(raw).not.toContain('hunter2');
    expect(raw).not.toMatch(/private_?key/i);
  });

  it('leaves out credential-like env entries of Kubernetes pod spec containers', () => {
    const config = {
      ...launchConfig('a'),
      environment: {
        image: { cpu: 'img' },
        pod_spec: {
          spec: {
            containers: [
              {
                env: [
                  { name: 'HF_TOKEN', value: 'hf-secret-value' },
                  { name: 'LANG', value: 'C.UTF-8' },
                ],
                name: 'determined-container',
              },
            ],
            initContainers: [{ env: [{ name: 'GIT_PASSWORD', value: 'git-secret-value' }] }],
          },
        },
      },
    };
    recordLaunch(1, { config, workspaceId: 1 });
    const [entry] = listLaunchHistory(1);
    expect(entry.redactedEnv).toEqual(['GIT_PASSWORD', 'HF_TOKEN']);
    expect(entry.config.environment.pod_spec.spec.containers[0].env).toEqual([
      { name: 'LANG', value: 'C.UTF-8' },
    ]);

    const raw = window.localStorage.getItem('u:1/launch-history/shell') ?? '';
    expect(raw).not.toContain('secret-value');
  });

  it('does nothing without a user, a config or a workspace', () => {
    recordLaunch(undefined, { config: launchConfig('a'), workspaceId: 1 });
    recordLaunch(1, { config: undefined, workspaceId: 1 });
    recordLaunch(1, { config: launchConfig('a') });
    expect(window.localStorage.length).toBe(0);
  });

  it('moves a repeated config to the front instead of duplicating it', () => {
    recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 });
    recordLaunch(1, { config: launchConfig('b'), workspaceId: 1 });
    recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 });
    expect(listLaunchHistory(1).map((entry) => entry.config.description)).toEqual(['a', 'b']);
  });

  it(`keeps at most ${LAUNCH_HISTORY_LIMIT} entries`, () => {
    for (let i = 0; i < LAUNCH_HISTORY_LIMIT + 5; i++) {
      recordLaunch(1, { config: launchConfig(`shell ${i}`), workspaceId: 1 });
    }
    const entries = listLaunchHistory(1);
    expect(entries).toHaveLength(LAUNCH_HISTORY_LIMIT);
    expect(entries[0].config.description).toBe(`shell ${LAUNCH_HISTORY_LIMIT + 4}`);
  });

  it('removes one entry or clears the list', () => {
    recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 });
    recordLaunch(1, { config: launchConfig('b'), workspaceId: 1 });
    const [newest] = listLaunchHistory(1);
    removeLaunchHistoryEntry(1, newest.id);
    expect(listLaunchHistory(1).map((entry) => entry.config.description)).toEqual(['a']);
    clearLaunchHistory(1);
    expect(listLaunchHistory(1)).toEqual([]);
    expect(window.localStorage.getItem('u:1/launch-history/shell')).toBeNull();
  });

  describe('storage failures', () => {
    it('reads corrupt or foreign data as an empty list or drops bad entries', () => {
      window.localStorage.setItem('u:1/launch-history/shell', '{not json');
      expect(listLaunchHistory(1)).toEqual([]);

      window.localStorage.setItem('u:1/launch-history/shell', JSON.stringify({ a: 1 }));
      expect(listLaunchHistory(1)).toEqual([]);

      recordLaunch(1, { config: launchConfig('good'), workspaceId: 1 });
      const stored = JSON.parse(window.localStorage.getItem('u:1/launch-history/shell') ?? '[]');
      window.localStorage.setItem(
        'u:1/launch-history/shell',
        JSON.stringify([{ v: 99 }, 'junk', ...stored]),
      );
      expect(listLaunchHistory(1).map((entry) => entry.config.description)).toEqual(['good']);
    });

    it('tolerates a getItem that throws', () => {
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new DOMException('denied', 'SecurityError');
      });
      expect(listLaunchHistory(1)).toEqual([]);
      expect(() => recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 })).not.toThrow();
    });

    it('tolerates a setItem that throws (quota exceeded)', () => {
      vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new DOMException('full', 'QuotaExceededError');
      });
      expect(() => recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 })).not.toThrow();
      expect(listLaunchHistory(1)).toEqual([]);
      expect(() => clearLaunchHistory(1)).not.toThrow();
    });

    it('tolerates window.localStorage itself throwing', () => {
      const descriptor = Object.getOwnPropertyDescriptor(window, 'localStorage');
      Object.defineProperty(window, 'localStorage', {
        configurable: true,
        get: () => {
          throw new DOMException('blocked', 'SecurityError');
        },
      });
      try {
        expect(listLaunchHistory(1)).toEqual([]);
        expect(() => recordLaunch(1, { config: launchConfig('a'), workspaceId: 1 })).not.toThrow();
        expect(() => clearLaunchHistory(1)).not.toThrow();
      } finally {
        if (descriptor) Object.defineProperty(window, 'localStorage', descriptor);
      }
    });
  });
});
