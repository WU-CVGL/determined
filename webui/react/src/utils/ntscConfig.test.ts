import { cloneDeep } from 'lodash';

import { RawJson } from 'types';

import {
  applyConfigEdits,
  configEdits,
  configFromForm,
  dataSourceFromEntry,
  DEFAULT_SOURCE_MODE,
  formFieldsFromConfig,
  isAutoDescription,
  minimalDiff,
  redactSensitiveEnv,
  relaunchConfig,
  sensitiveEnvNames,
  ShellServiceChoices,
  simpleLaunchConfig,
  sourceIds,
  sshDisabled,
  stableStringify,
  templateFromConfig,
  templateResources,
  TensorBoardDataSource,
  withDataSource,
} from './ntscConfig';

const mergedShellConfig = {
  bind_mounts: [{ container_path: '/data', host_path: '/mnt/data', read_only: false }],
  debug: false,
  description: 'Shell (gently-brave-otter)',
  entrypoint: ['/run/determined/ssh/shell-entrypoint.sh', '-f', 'sshd_config', '-p', '3201'],
  environment: {
    environment_variables: { cpu: ['HF_TOKEN=abc', 'LANG=C.UTF-8'], cuda: ['HF_TOKEN=abc'] },
    image: { cpu: 'cpu-image:1', cuda: 'cuda-image:1' },
    registry_auth: { password: 'hunter2', username: 'me' },
  },
  idle_timeout: null,
  resources: { priority: 42, resource_pool: 'gpu', slots: 2 },
  work_dir: null,
};

/** A Kubernetes pod spec with credentials written out in container env entries. */
const podSpecConfig = {
  environment: {
    pod_spec: {
      apiVersion: 'v1',
      kind: 'Pod',
      spec: {
        containers: [
          {
            env: [
              { name: 'HF_TOKEN', value: 'hf-secret-value' },
              { name: 'LANG', value: 'C.UTF-8' },
              { name: 'WANDB_API_KEY', valueFrom: { secretKeyRef: { key: 'key', name: 'wandb' } } },
            ],
            name: 'determined-container',
          },
        ],
        initContainers: [
          { env: [{ name: 'GIT_PASSWORD', value: 'git-secret-value' }], name: 'fetch-code' },
        ],
      },
    },
  },
};

describe('ntscConfig', () => {
  describe('relaunchConfig', () => {
    it('deletes launch-specific and secret keys and keeps the rest', () => {
      const out = relaunchConfig(mergedShellConfig);
      expect(out).not.toHaveProperty('entrypoint');
      expect(out).not.toHaveProperty('description');
      expect(out.environment).not.toHaveProperty('registry_auth');
      expect(out.resources).not.toHaveProperty('priority');
      expect(out.resources).toEqual({ resource_pool: 'gpu', slots: 2 });
      expect(out.bind_mounts).toEqual(mergedShellConfig.bind_mounts);
      expect(out.environment.image).toEqual(mergedShellConfig.environment.image);
      expect(out.environment.environment_variables).toEqual(
        mergedShellConfig.environment.environment_variables,
      );
    });

    it('keeps a description the user chose', () => {
      expect(relaunchConfig({ description: 'my shell' }).description).toBe('my shell');
    });

    it('drops the checkpoint storage mount of a tensorboard service that reads experiments', () => {
      const sharedFs = {
        container_path: '/determined_shared_fs',
        host_path: '/mnt/ckpt-a',
        propagation: 'rprivate',
        read_only: true,
      };
      const session = {
        ...mergedShellConfig,
        bind_mounts: [...mergedShellConfig.bind_mounts, sharedFs],
        services: { tensorboard: { trial_ids: [5] } },
      };
      expect(relaunchConfig(session).bind_mounts).toEqual(mergedShellConfig.bind_mounts);
      const onlyShared = { ...session, bind_mounts: [sharedFs] };
      expect(relaunchConfig(onlyShared)).not.toHaveProperty('bind_mounts');
      // A log directory reads no checkpoint storage: the mount is the user's.
      const logdirShell = { ...session, services: { tensorboard: { logdir: 'runs' } } };
      expect(relaunchConfig(logdirShell).bind_mounts).toEqual(session.bind_mounts);
    });

    it('does not change its input', () => {
      const input = cloneDeep(mergedShellConfig);
      relaunchConfig(input);
      expect(input).toEqual(mergedShellConfig);
    });
  });

  describe('configEdits and applyConfigEdits', () => {
    /** The request of a "Launch Again" of mergedShellConfig, which the master previews as it. */
    const request = relaunchConfig(mergedShellConfig);
    /** Applies the edits that turn previewed into edited to the request. */
    const draft = (
      edited: RawJson,
      previewed: RawJson = mergedShellConfig,
      base: RawJson = request,
    ) => applyConfigEdits(base, edited, previewed);

    it('leaves the request as it is when nothing was edited', () => {
      expect(configEdits(mergedShellConfig, mergedShellConfig)).toEqual({
        changed: {},
        removed: [],
      });
      expect(draft(mergedShellConfig)).toEqual(request);
    });

    it('sets a changed value and keeps the values the master filled in out', () => {
      const edited = {
        ...mergedShellConfig,
        resources: { ...mergedShellConfig.resources, slots: 4 },
      };
      expect(configEdits(edited, mergedShellConfig)).toEqual({
        changed: { resources: { slots: 4 } },
        removed: [],
      });
      const out = draft(edited);
      expect(out.resources).toEqual({ resource_pool: 'gpu', slots: 4 });
      expect(out).not.toHaveProperty('entrypoint');
      expect(out.environment).not.toHaveProperty('registry_auth');
    });

    it('sets an added value, also under a key the request does not have', () => {
      const registryAuth = { password: 'other', username: 'you' };
      const edited = {
        ...mergedShellConfig,
        environment: { ...mergedShellConfig.environment, registry_auth: registryAuth },
        resources: { ...mergedShellConfig.resources, priority: 7 },
        services: { tensorboard: { args: ['--x'], logdir: 'runs' } },
      };
      const out = draft(edited, mergedShellConfig, { description: 'lab' });
      expect(out).toEqual({
        description: 'lab',
        environment: { registry_auth: registryAuth },
        resources: { priority: 7 },
        services: { tensorboard: { args: ['--x'], logdir: 'runs' } },
      });
    });

    it('deletes a removed key that the request has and ignores one it does not', () => {
      const base = { ...request, description: 'mine', resources: { slots: 2 } };
      const previewed = { ...mergedShellConfig, description: 'mine' };
      const edited: RawJson = cloneDeep(previewed);
      delete edited.description;
      delete edited.environment.registry_auth;
      delete edited.resources.priority;
      delete edited.resources.slots;
      expect(configEdits(edited, previewed).removed).toEqual([
        ['description'],
        ['environment', 'registry_auth'],
        ['resources', 'priority'],
        ['resources', 'slots'],
      ]);
      const out = draft(edited, previewed, base);
      expect(out).not.toHaveProperty('description');
      expect(out.resources).toEqual({});
      expect(out.environment).toEqual(request.environment);
      // A removed section whose keys the request partly has.
      const { bind_mounts: _mounts, ...withoutMounts } = previewed;
      expect(draft(withoutMounts, previewed, base)).not.toHaveProperty('bind_mounts');
      expect(draft(withoutMounts, previewed, {})).toEqual({});
    });

    it('recurses into nested objects', () => {
      const previewed = { a: { b: { c: 1, d: 2 }, e: 3 } };
      const edited = { a: { b: { c: 1, d: 5, f: 6 }, e: 3 } };
      expect(configEdits(edited, previewed).changed).toEqual({ a: { b: { d: 5, f: 6 } } });
      expect(draft(edited, previewed, { a: { b: { c: 9 } }, g: 1 })).toEqual({
        a: { b: { c: 9, d: 5, f: 6 } },
        g: 1,
      });
      const removed = { a: { b: { c: 1 }, e: 3 } };
      expect(draft(removed, previewed, { a: { b: { c: 9, d: 2 } } })).toEqual({
        a: { b: { c: 9 } },
      });
    });

    it('sets an edited array as a whole', () => {
      const mount = { container_path: '/scratch', host_path: '/mnt/scratch' };
      const edited = {
        ...mergedShellConfig,
        bind_mounts: [...mergedShellConfig.bind_mounts, mount],
        environment: {
          ...mergedShellConfig.environment,
          environment_variables: {
            ...mergedShellConfig.environment.environment_variables,
            cuda: [],
          },
        },
      };
      const out = draft(edited, mergedShellConfig, {});
      expect(out).toEqual({
        bind_mounts: edited.bind_mounts,
        environment: { environment_variables: { cuda: [] } },
      });
    });

    it('sets a null that the user typed, changed or added', () => {
      const edited = {
        ...mergedShellConfig,
        debug: null,
        services: { jupyter: null, vscode: { opt: null } },
        work_dir: null,
      };
      expect(configEdits(edited, mergedShellConfig)).toEqual({
        changed: { debug: null, services: { jupyter: null, vscode: { opt: null } } },
        removed: [],
      });
      const out = draft(edited);
      expect(out.debug).toBeNull();
      expect(out.services).toEqual({ jupyter: null, vscode: { opt: null } });
      // The previewed null is no edit.
      expect(draft(edited, mergedShellConfig, {})).not.toHaveProperty('work_dir');
      // A null the user removes is removed where the request has it.
      const { idle_timeout: _timeout, ...withoutTimeout } = mergedShellConfig;
      expect(draft(withoutTimeout, mergedShellConfig, { idle_timeout: null })).toEqual({});
    });

    it('sets an edited object whole where the request holds a value that is no object', () => {
      // The master expands an image given as one string into one per runtime.
      const base = { environment: { image: 'my/img:1' } };
      const previewed = {
        environment: { image: { cpu: 'my/img:1', cuda: 'my/img:1', rocm: 'my/img:1' } },
      };
      const edited = cloneDeep(previewed);
      edited.environment.image.cuda = 'my/img:2';
      expect(draft(edited, previewed, base)).toEqual(edited);
      // Where the request has none, only the changed key is set.
      expect(draft(edited, previewed, {})).toEqual({
        environment: { image: { cuda: 'my/img:2' } },
      });
    });

    it('does not change its inputs', () => {
      const base = cloneDeep(request);
      const edited = { ...mergedShellConfig, debug: true };
      const out = applyConfigEdits(base, edited, mergedShellConfig);
      expect(base).toEqual(request);
      expect(edited.debug).toBe(true);
      out.resources.slots = 9;
      expect(base.resources.slots).toBe(2);
    });
  });

  describe('templateFromConfig', () => {
    it('drops description, secrets and the nulls that clear no default', () => {
      const out = templateFromConfig(mergedShellConfig, {});
      expect(out).not.toHaveProperty('description');
      expect(out).not.toHaveProperty('entrypoint');
      expect(out).not.toHaveProperty('idle_timeout');
      expect(out).not.toHaveProperty('work_dir');
      expect(out.environment).not.toHaveProperty('registry_auth');
      expect(out.resources).toEqual({ resource_pool: 'gpu', slots: 2 });
    });

    it('keeps only what differs from the defaults, plus pool and slots', () => {
      const defaults = {
        ...mergedShellConfig,
        description: 'Shell (other-pet-name)',
        environment: { ...mergedShellConfig.environment, image: { cpu: 'cpu-image:1' } },
      };
      expect(templateFromConfig(mergedShellConfig, defaults)).toEqual({
        environment: { image: { cuda: 'cuda-image:1' } },
        resources: { resource_pool: 'gpu', slots: 2 },
      });
    });

    it('keeps a service turned off, also where the defaults do not run it', () => {
      const config = { services: { jupyter: {}, vscode: { enabled: false } } };
      // The cluster may run it later: the template keeps it off.
      expect(templateFromConfig(config, { services: { jupyter: {} } })).toEqual({
        services: { vscode: { enabled: false } },
      });
      expect(templateFromConfig(config, { services: { jupyter: {}, vscode: {} } })).toEqual({
        services: { vscode: { enabled: false } },
      });
      expect(templateFromConfig(config, { services: { jupyter: {}, vscode: null } })).toEqual({
        services: { vscode: { enabled: false } },
      });
      expect(templateFromConfig(config, {})).toEqual({
        services: { jupyter: {}, vscode: { enabled: false } },
      });
      // A service with other options keeps them.
      expect(
        templateFromConfig(
          { services: { vscode: { enabled: false, opt: 1 } } },
          { services: { jupyter: {} } },
        ),
      ).toEqual({ services: { vscode: { enabled: false, opt: 1 } } });
      // A service that runs as the defaults run it is not a setting.
      expect(
        templateFromConfig({ services: { jupyter: {} } }, { services: { jupyter: {} } }),
      ).toEqual({});
    });

    it('keeps a null work_dir that clears the default one', () => {
      const defaults = { ...mergedShellConfig, work_dir: '/cluster/default-dir' };
      expect(templateFromConfig(mergedShellConfig, defaults)).toEqual({
        resources: { resource_pool: 'gpu', slots: 2 },
        work_dir: null,
      });
    });

    it('keeps a nested null that clears a default', () => {
      const devices = [{ container_path: '/dev/fuse', host_path: '/dev/fuse', mode: 'mrw' }];
      const defaults = {
        ...mergedShellConfig,
        resources: { ...mergedShellConfig.resources, devices },
      };
      const config = {
        ...mergedShellConfig,
        resources: { ...mergedShellConfig.resources, devices: null },
      };
      expect(templateFromConfig(config, defaults)).toEqual({
        resources: { devices: null, resource_pool: 'gpu', slots: 2 },
      });
    });
  });

  describe('minimalDiff', () => {
    it('compares objects key by key and arrays as a whole', () => {
      expect(
        minimalDiff(
          { a: 1, b: { c: 2, d: 3 }, e: [1, 2], f: 'x' },
          { a: 1, b: { c: 2, d: 4 }, e: [1], f: 'x' },
        ),
      ).toEqual({ b: { d: 3 }, e: [1, 2] });
    });

    it('keeps a null only where the default is set', () => {
      expect(
        minimalDiff(
          { a: null, b: null, c: null, d: { e: null, f: 1 }, g: { h: null, i: 1 } },
          { a: 'x', b: null, d: { e: 2, f: 1 } },
        ),
      ).toEqual({ a: null, d: { e: null }, g: { i: 1 } });
    });

    it('keeps every null but one where the default is null, with keepNulls', () => {
      expect(
        minimalDiff(
          { a: null, b: null, c: null, d: { e: null, f: 1 }, g: { h: null, i: 1 } },
          { a: 'x', b: null, d: { e: 2, f: 1 } },
          { keepNulls: true },
        ),
      ).toEqual({ a: null, c: null, d: { e: null }, g: { h: null, i: 1 } });
    });
  });

  describe('form fields', () => {
    it('reads name, pool, slots and services, ignoring a generated name', () => {
      expect(formFieldsFromConfig(mergedShellConfig)).toEqual({
        dataSource: { mode: DEFAULT_SOURCE_MODE },
        name: '',
        pool: 'gpu',
        services: {},
        slots: 2,
      });
      expect(formFieldsFromConfig({ description: 'mine' }).name).toBe('mine');
      // The known services besides ssh, which every shell runs, that have an entry: whether the
      // entry runs them. The master's empty entry can be null.
      expect(
        formFieldsFromConfig({ services: { jupyter: {}, other: {}, ssh: {}, vscode: null } })
          .services,
      ).toEqual({ jupyter: true, vscode: true });
      expect(
        formFieldsFromConfig({
          services: { jupyter: { enabled: false }, vscode: { enabled: true, opt: 1 } },
        }).services,
      ).toEqual({ jupyter: false, vscode: true });
      expect(formFieldsFromConfig({ services: { vscode: {} } }).services).toEqual({
        vscode: true,
      });
    });

    it('sends the known services with a choice, and no services key without them', () => {
      expect(simpleLaunchConfig({ services: { jupyter: true, vscode: true }, slots: 1 })).toEqual({
        description: undefined,
        resources: { resource_pool: undefined, slots: 1 },
        services: { jupyter: {}, vscode: {} },
      });
      expect(simpleLaunchConfig({ services: { jupyter: true, vscode: false } }).services).toEqual({
        jupyter: {},
        vscode: { enabled: false },
      });
      // A service without a choice gets no entry.
      expect(simpleLaunchConfig({ services: { jupyter: false } }).services).toEqual({
        jupyter: { enabled: false },
      });
      expect(
        simpleLaunchConfig({ services: { other: true, ssh: false } as ShellServiceChoices }),
      ).not.toHaveProperty('services');
      expect(simpleLaunchConfig({ services: {} })).not.toHaveProperty('services');
      expect(simpleLaunchConfig({})).not.toHaveProperty('services');
    });

    it('applies the services on top of a base config, keeping the others and their options', () => {
      const base = { services: { jupyter: { enabled: false, opt: 1 }, other: {} } };
      expect(configFromForm(base, { services: { jupyter: true, vscode: true } }).services).toEqual({
        jupyter: { opt: 1 },
        other: {},
        vscode: {},
      });
      expect(
        configFromForm(base, { services: { jupyter: false, vscode: false } }).services,
      ).toEqual({
        jupyter: { enabled: false, opt: 1 },
        other: {},
        vscode: { enabled: false },
      });
      expect(
        configFromForm(
          { services: { jupyter: {}, vscode: { opt: 2 } } },
          { services: { jupyter: false, vscode: false } },
        ).services,
      ).toEqual({ jupyter: { enabled: false }, vscode: { enabled: false, opt: 2 } });
      // A service without a choice keeps the config's entry, or has none.
      expect(configFromForm({}, { services: { jupyter: true } }).services).toEqual({
        jupyter: {},
      });
      expect(configFromForm(base, { services: { vscode: true } }).services).toEqual({
        ...base.services,
        vscode: {},
      });
      // Without services in the fields, the config's services stay as they are.
      expect(configFromForm(base, {}).services).toEqual(base.services);
      expect(configFromForm({}, { services: {} })).not.toHaveProperty('services');
      // The input is not changed.
      expect(base.services).toEqual({ jupyter: { enabled: false, opt: 1 }, other: {} });
    });

    it('carries the log directory of the tensorboard service', () => {
      const logdir = (dir: string): TensorBoardDataSource => ({ logdir: dir, mode: 'logdir' });
      expect(
        formFieldsFromConfig({ services: { tensorboard: { args: ['--x'], logdir: 'runs' } } }),
      ).toMatchObject({ dataSource: logdir('runs'), services: { tensorboard: true } });
      expect(
        formFieldsFromConfig({ services: { tensorboard: { enabled: false, logdir: 'runs' } } }),
      ).toMatchObject({ dataSource: logdir('runs'), services: { tensorboard: false } });
      expect(formFieldsFromConfig({ services: { jupyter: {} } }).dataSource).toEqual({
        mode: DEFAULT_SOURCE_MODE,
      });

      // Sent with the service that runs, never on its own or with one that does not.
      expect(
        simpleLaunchConfig({
          dataSource: logdir('runs'),
          services: { jupyter: true, tensorboard: true },
        }).services,
      ).toEqual({ jupyter: {}, tensorboard: { logdir: 'runs' } });
      expect(
        simpleLaunchConfig({ dataSource: logdir('runs'), services: { tensorboard: false } })
          .services,
      ).toEqual({ tensorboard: { enabled: false } });
      expect(simpleLaunchConfig({ dataSource: logdir('runs') })).not.toHaveProperty('services');
      expect(
        simpleLaunchConfig({ dataSource: logdir('runs'), services: { jupyter: true } }).services,
      ).toEqual({ jupyter: {} });

      // Over a base config: the field replaces the entry's logdir, the entry's args stay.
      const base = { services: { tensorboard: { args: ['--x'], enabled: false, logdir: 'old' } } };
      expect(
        configFromForm(base, { dataSource: logdir('new'), services: { tensorboard: true } })
          .services,
      ).toEqual({ tensorboard: { args: ['--x'], logdir: 'new' } });
      expect(configFromForm(base, { services: { tensorboard: true } }).services).toEqual({
        tensorboard: { args: ['--x'], logdir: 'old' },
      });
      expect(
        configFromForm(base, { dataSource: logdir(''), services: { tensorboard: true } }).services,
      ).toEqual({ tensorboard: { args: ['--x'], logdir: 'old' } });
      expect(
        configFromForm(base, { dataSource: logdir('new'), services: { tensorboard: false } })
          .services,
      ).toEqual(base.services);
      expect(configFromForm(base, { dataSource: logdir('new') }).services).toEqual(base.services);
    });

    it('keeps the tensorboard service and its options in a template', () => {
      const config = {
        services: { jupyter: {}, tensorboard: { args: ['--x'], logdir: 'runs' } },
      };
      expect(templateFromConfig(config, { services: { jupyter: {} } })).toEqual({
        services: { tensorboard: { args: ['--x'], logdir: 'runs' } },
      });
    });

    it('applies the form on top of a base config without touching other keys', () => {
      const out = configFromForm(relaunchConfig(mergedShellConfig), {
        name: 'again',
        pool: 'cpu',
        slots: 0,
      });
      expect(out.description).toBe('again');
      expect(out.resources).toEqual({ resource_pool: 'cpu', slots: 0 });
      expect(out.bind_mounts).toEqual(mergedShellConfig.bind_mounts);

      const cleared = configFromForm({ description: 'x', resources: { resource_pool: 'a' } }, {});
      expect(cleared).toEqual({ resources: {} });
    });

    it('reads an edited config into the form and back without changing it', () => {
      const edited = {
        ...mergedShellConfig,
        description: 'my shell',
        services: {
          jupyter: { enabled: false },
          tensorboard: { args: ['--reload_interval=30'], logdir: 'runs' },
          vscode: {},
        },
      };
      expect(configFromForm(edited, formFieldsFromConfig(edited))).toEqual(edited);
    });

    it('reads template resources', () => {
      expect(templateResources({ resources: { resource_pool: 'gpu', slots: 4 } })).toEqual({
        pool: 'gpu',
        slots: 4,
      });
      expect(templateResources(undefined)).toEqual({ pool: undefined, slots: undefined });
      // A template that turns ssh off runs with 0 slots, with or without slots of its own.
      expect(templateResources({ services: { ssh: { enabled: false } } })).toEqual({
        pool: undefined,
        slots: 0,
      });
      expect(
        templateResources({
          resources: { resource_pool: 'aux', slots: 2 },
          services: { ssh: { enabled: false } },
        }),
      ).toEqual({ pool: 'aux', slots: 0 });
    });
  });

  describe('tensorboard data source', () => {
    const experiments = (
      experimentIds: (number | string)[] = [],
      trialIds: (number | string)[] = [],
    ): TensorBoardDataSource => ({ experimentIds, mode: 'experiments', trialIds });
    const logdir = (dir: string): TensorBoardDataSource => ({ logdir: dir, mode: 'logdir' });

    /** A TensorBoard viewing session's merged config, as the master returns it. */
    const sessionConfig = {
      description: 'TensorBoard (brave-cat)',
      entrypoint: ['/run/determined/ssh/shell-entrypoint.sh'],
      environment: { image: { cpu: 'cpu-image:1' } },
      idle_timeout: '5m',
      resources: { priority: 42, resource_pool: 'aux', slots: 0 },
      services: { ssh: { enabled: false }, tensorboard: { experiment_ids: [3, 7] } },
    };

    it('reads positive integer IDs, sorted numerically, without duplicates', () => {
      expect(sourceIds([10, '9', 9, ' 3 ', '0', '-1', '1.5', '12a', ''])).toEqual([3, 9, 10]);
      expect(sourceIds(undefined)).toEqual([]);
    });

    it('tells whether a config turns ssh off', () => {
      expect(sshDisabled({ services: { ssh: { enabled: false } } })).toBe(true);
      expect(sshDisabled({ services: { ssh: {} } })).toBe(false);
      expect(sshDisabled({ services: { ssh: null } })).toBe(false);
      expect(sshDisabled({})).toBe(false);
      expect(sshDisabled(undefined)).toBe(false);
    });

    it('reads the source of an entry: experiments with IDs, a log directory, or the default', () => {
      expect(dataSourceFromEntry({ experiment_ids: [1], trial_ids: [2] })).toEqual(
        experiments([1], [2]),
      );
      expect(dataSourceFromEntry({ trial_ids: [2] })).toEqual(experiments([], [2]));
      expect(dataSourceFromEntry({ args: ['--x'], logdir: 'runs' })).toEqual(logdir('runs'));
      expect(dataSourceFromEntry({})).toEqual({ mode: DEFAULT_SOURCE_MODE });
      expect(dataSourceFromEntry(null)).toEqual({ mode: DEFAULT_SOURCE_MODE });
      expect(dataSourceFromEntry(undefined)).toEqual({ mode: 'experiments' });
    });

    it('keeps exactly one source in an entry, and its other options', () => {
      const ids = { args: ['--x'], experiment_ids: [1], trial_ids: [2] };
      expect(withDataSource(ids, logdir('runs'))).toEqual({ args: ['--x'], logdir: 'runs' });
      expect(withDataSource({ args: ['--x'], logdir: 'old' }, experiments([7, 3, 7]))).toEqual({
        args: ['--x'],
        experiment_ids: [3, 7],
      });
      expect(withDataSource(ids, experiments([], ['5']))).toEqual({
        args: ['--x'],
        trial_ids: [5],
      });
      expect(withDataSource(ids, experiments())).toEqual({ args: ['--x'] });
      // A blank log directory keeps the entry's.
      expect(withDataSource({ logdir: 'old' }, logdir(' '))).toEqual({ logdir: 'old' });
      // The input is not changed.
      expect(ids).toEqual({ args: ['--x'], experiment_ids: [1], trial_ids: [2] });
    });

    it('keeps the source of a tensorboard service that does not run', () => {
      const base = { services: { tensorboard: { experiment_ids: [1] } } };
      expect(
        configFromForm(base, { dataSource: logdir('runs'), services: { tensorboard: false } })
          .services,
      ).toEqual({ tensorboard: { enabled: false, experiment_ids: [1] } });
    });

    it('sends exactly one source for every base and form mode', () => {
      const bases: Record<string, unknown> = {
        experiments: { args: ['--x'], experiment_ids: [1], trial_ids: [2] },
        logdir: { args: ['--x'], logdir: 'old' },
        none: undefined,
      };
      const forms: Record<string, TensorBoardDataSource> = {
        experiments: experiments(['7']),
        logdir: logdir('new'),
      };
      Object.entries(bases).forEach(([, entry]) => {
        const base = entry === undefined ? {} : { services: { tensorboard: entry } };
        expect(
          configFromForm(base, { dataSource: forms.logdir, services: { tensorboard: true } })
            .services.tensorboard,
        ).toEqual(entry === undefined ? { logdir: 'new' } : { args: ['--x'], logdir: 'new' });
        expect(
          configFromForm(base, { dataSource: forms.experiments, services: { tensorboard: true } })
            .services.tensorboard,
        ).toEqual(
          entry === undefined ? { experiment_ids: [7] } : { args: ['--x'], experiment_ids: [7] },
        );
      });
      expect(
        simpleLaunchConfig({ dataSource: forms.experiments, services: { tensorboard: true } })
          .services,
      ).toEqual({ tensorboard: { experiment_ids: [7] } });
      expect(
        simpleLaunchConfig({ dataSource: forms.logdir, services: { tensorboard: true } }).services,
      ).toEqual({ tensorboard: { logdir: 'new' } });
    });

    it('runs a config that turns ssh off with 0 slots', () => {
      const base = { resources: { slots: 2 }, services: { ssh: { enabled: false } } };
      expect(configFromForm(base, { slots: 4 }).resources).toEqual({ slots: 0 });
      expect(configFromForm(base, {}).resources).toEqual({ slots: 0 });
      expect(configFromForm({ resources: { slots: 2 } }, { slots: 4 }).resources).toEqual({
        slots: 4,
      });
    });

    it('drops the name the master gave a TensorBoard session, not one a user gave', () => {
      expect(isAutoDescription('TensorBoard (brave-cat)')).toBe(true);
      expect(isAutoDescription('Shell (gently-brave-otter)')).toBe(true);
      expect(isAutoDescription('TensorBoard of exp 7')).toBe(false);
      expect(relaunchConfig(sessionConfig)).not.toHaveProperty('description');
      expect(formFieldsFromConfig(sessionConfig).name).toBe('');
    });

    it('launches a session, a log directory shell and a plain shell again as they were', () => {
      const logdirShell = {
        ...mergedShellConfig,
        services: { jupyter: {}, tensorboard: { args: ['--x'], logdir: 'runs' } },
      };
      [sessionConfig, logdirShell, mergedShellConfig].forEach((config) => {
        const base = relaunchConfig(config);
        expect(configFromForm(base, formFieldsFromConfig(base))).toEqual(base);
      });
      expect(formFieldsFromConfig(relaunchConfig(sessionConfig))).toEqual({
        dataSource: experiments([3, 7], []),
        name: '',
        pool: 'aux',
        services: { tensorboard: true },
        slots: 0,
      });
    });

    it('applies the edits of a session config and reads its source back unchanged', () => {
      const edited = { ...sessionConfig, description: 'my session', idle_timeout: '10m' };
      const base = relaunchConfig(sessionConfig);
      const draft = applyConfigEdits(base, edited, sessionConfig);
      expect(draft).toEqual({ ...base, description: 'my session', idle_timeout: '10m' });
      const fields = formFieldsFromConfig(draft);
      expect(fields.dataSource).toEqual(experiments([3, 7], []));
      expect(sshDisabled(draft)).toBe(true);
      expect(configFromForm(draft, fields)).toEqual(draft);
      // Another round trip without edits changes nothing.
      const request = configFromForm(draft, fields);
      const again = applyConfigEdits(request, edited, edited);
      expect(configFromForm(again, formFieldsFromConfig(again))).toEqual(draft);
      // An ID added in the form joins the draft's; ssh stays off with 0 slots.
      const added = configFromForm(draft, { ...fields, dataSource: experiments([3, 7, '12']) });
      expect(added.services).toEqual({
        ssh: { enabled: false },
        tensorboard: { experiment_ids: [3, 7, 12] },
      });
      expect(added.resources).toEqual({ resource_pool: 'aux', slots: 0 });
    });

    it('reads the mode of an edited draft and sends only the chosen one', () => {
      const previewed = {
        ...mergedShellConfig,
        services: { tensorboard: { args: ['--x'], logdir: 'runs' } },
      };
      const edited = { ...previewed, services: { tensorboard: { args: ['--x'], trial_ids: [5] } } };
      const draft = applyConfigEdits(relaunchConfig(previewed), edited, previewed);
      const fields = formFieldsFromConfig(draft);
      expect(fields.dataSource).toEqual(experiments([], [5]));
      expect(configFromForm(draft, fields).services).toEqual(edited.services);
      // Back to a log directory: no IDs are left.
      expect(configFromForm(draft, { ...fields, dataSource: logdir('logs') }).services).toEqual({
        tensorboard: { args: ['--x'], logdir: 'logs' },
      });
      // From a log directory draft to IDs: no log directory is left.
      const logdirDraft = applyConfigEdits(relaunchConfig(edited), previewed, edited);
      const logdirFields = formFieldsFromConfig(logdirDraft);
      expect(logdirFields.dataSource).toEqual(logdir('runs'));
      expect(
        configFromForm(logdirDraft, { ...logdirFields, dataSource: experiments([4]) }).services,
      ).toEqual({ tensorboard: { args: ['--x'], experiment_ids: [4] } });
    });

    it('keeps ssh off, the sources, 0 slots and the idle timeout of a session in a template', () => {
      const defaults = {
        environment: { image: { cpu: 'cpu-image:1' } },
        idle_timeout: null,
        resources: { priority: 42, resource_pool: 'aux', slots: 0 },
        services: { jupyter: {} },
      };
      expect(templateFromConfig(sessionConfig, defaults)).toEqual({
        idle_timeout: '5m',
        resources: { resource_pool: 'aux', slots: 0 },
        services: { ssh: { enabled: false }, tensorboard: { experiment_ids: [3, 7] } },
      });
    });
  });

  describe('sensitive environment variables', () => {
    it('finds credential-like names in lists and per-runtime maps', () => {
      expect(sensitiveEnvNames(mergedShellConfig)).toEqual(['HF_TOKEN']);
      expect(
        sensitiveEnvNames({
          environment: { environment_variables: ['MY_PASSWORD=x', 'AWS_SECRET_KEY=y', 'A=1'] },
        }),
      ).toEqual(['AWS_SECRET_KEY', 'MY_PASSWORD']);
      expect(sensitiveEnvNames({})).toEqual([]);
    });

    it('removes them and reports their names', () => {
      const { config, redacted } = redactSensitiveEnv(mergedShellConfig);
      expect(redacted).toEqual(['HF_TOKEN']);
      expect(config.environment.environment_variables).toEqual({
        cpu: ['LANG=C.UTF-8'],
        cuda: [],
      });
      expect(mergedShellConfig.environment.environment_variables.cpu).toContain('HF_TOKEN=abc');
    });

    it('finds written-out values in pod spec container env, not valueFrom references', () => {
      expect(sensitiveEnvNames(podSpecConfig)).toEqual(['GIT_PASSWORD', 'HF_TOKEN']);
      expect(
        sensitiveEnvNames({
          environment: {
            environment_variables: ['MY_PASSWORD=x'],
            pod_spec: { spec: { containers: [{ env: [{ name: 'AWS_SECRET_KEY', value: 'y' }] }] } },
          },
        }),
      ).toEqual(['AWS_SECRET_KEY', 'MY_PASSWORD']);
      expect(
        sensitiveEnvNames({ environment: { pod_spec: { spec: { containers: 'x' } } } }),
      ).toEqual([]);
    });

    it('removes them from pod spec containers and init containers', () => {
      const { config, redacted } = redactSensitiveEnv(podSpecConfig);
      expect(redacted).toEqual(['GIT_PASSWORD', 'HF_TOKEN']);
      const { spec } = config.environment.pod_spec;
      expect(spec.containers[0].env).toEqual([
        { name: 'LANG', value: 'C.UTF-8' },
        { name: 'WANDB_API_KEY', valueFrom: { secretKeyRef: { key: 'key', name: 'wandb' } } },
      ]);
      expect(spec.initContainers[0]).toEqual({ env: [], name: 'fetch-code' });
      expect(JSON.stringify(config)).not.toMatch(/secret-value/);
      expect(JSON.stringify(podSpecConfig)).toContain('hf-secret-value');
    });
  });

  it('stableStringify ignores key order', () => {
    const reversed = JSON.parse('{"b":{"c":[1,{"e":3,"d":2}]},"a":1}');
    expect(stableStringify({ a: 1, b: { c: [1, { d: 2, e: 3 }] } })).toBe(
      stableStringify(reversed),
    );
    expect(stableStringify(reversed)).toBe('{"a":1,"b":{"c":[1,{"d":2,"e":3}]}}');
  });
});
