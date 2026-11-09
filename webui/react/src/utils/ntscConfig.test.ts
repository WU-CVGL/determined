import { cloneDeep } from 'lodash';

import {
  configFromForm,
  formFieldsFromConfig,
  minimalDiff,
  redactSensitiveEnv,
  relaunchConfig,
  sensitiveEnvNames,
  ShellServiceChoices,
  simpleLaunchConfig,
  stableStringify,
  templateFromConfig,
  templateResources,
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

    it('does not change its input', () => {
      const input = cloneDeep(mergedShellConfig);
      relaunchConfig(input);
      expect(input).toEqual(mergedShellConfig);
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
  });

  describe('form fields', () => {
    it('reads name, pool, slots and services, ignoring a generated name', () => {
      expect(formFieldsFromConfig(mergedShellConfig)).toEqual({
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
      expect(
        formFieldsFromConfig({ services: { tensorboard: { args: ['--x'], logdir: 'runs' } } }),
      ).toMatchObject({ logdir: 'runs', services: { tensorboard: true } });
      expect(
        formFieldsFromConfig({ services: { tensorboard: { enabled: false, logdir: 'runs' } } }),
      ).toMatchObject({ logdir: 'runs', services: { tensorboard: false } });
      expect(formFieldsFromConfig({ services: { jupyter: {} } }).logdir).toBeUndefined();

      // Sent with the service that runs, never on its own or with one that does not.
      expect(
        simpleLaunchConfig({ logdir: 'runs', services: { jupyter: true, tensorboard: true } })
          .services,
      ).toEqual({ jupyter: {}, tensorboard: { logdir: 'runs' } });
      expect(
        simpleLaunchConfig({ logdir: 'runs', services: { tensorboard: false } }).services,
      ).toEqual({ tensorboard: { enabled: false } });
      expect(simpleLaunchConfig({ logdir: 'runs' })).not.toHaveProperty('services');
      expect(simpleLaunchConfig({ logdir: 'runs', services: { jupyter: true } }).services).toEqual({
        jupyter: {},
      });

      // Over a base config: the field replaces the entry's logdir, the entry's args stay.
      const base = { services: { tensorboard: { args: ['--x'], enabled: false, logdir: 'old' } } };
      expect(
        configFromForm(base, { logdir: 'new', services: { tensorboard: true } }).services,
      ).toEqual({ tensorboard: { args: ['--x'], logdir: 'new' } });
      expect(configFromForm(base, { services: { tensorboard: true } }).services).toEqual({
        tensorboard: { args: ['--x'], logdir: 'old' },
      });
      expect(
        configFromForm(base, { logdir: 'new', services: { tensorboard: false } }).services,
      ).toEqual(base.services);
      expect(configFromForm(base, { logdir: 'new' }).services).toEqual(base.services);
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
