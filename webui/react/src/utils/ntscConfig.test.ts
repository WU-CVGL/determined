import { cloneDeep } from 'lodash';

import {
  configFromForm,
  formFieldsFromConfig,
  minimalDiff,
  redactSensitiveEnv,
  sanitizeConfig,
  sensitiveEnvNames,
  stableStringify,
  templateFromConfig,
  templateResources,
  toShellConfig,
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
  notebook_idle_type: 'kernels_or_terminals',
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
  describe('sanitizeConfig', () => {
    it('deletes launch-specific and secret keys and keeps the rest', () => {
      const out = sanitizeConfig(mergedShellConfig);
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
      expect(sanitizeConfig({ description: 'my shell' }).description).toBe('my shell');
      expect(sanitizeConfig({ description: 'JupyterLab (a-b-c)' })).not.toHaveProperty(
        'description',
      );
    });

    it('does not change its input', () => {
      const input = cloneDeep(mergedShellConfig);
      sanitizeConfig(input);
      expect(input).toEqual(mergedShellConfig);
    });
  });

  describe('toShellConfig', () => {
    it('removes the notebook-only keys of a JupyterLab preview', () => {
      const out = toShellConfig({
        description: 'JupyterLab (kindly-quick-heron)',
        entrypoint: null,
        environment: { image: { cpu: 'img' } },
        idle_timeout: '30m',
        notebook_idle_type: 'kernels_or_terminals',
        resources: { resource_pool: 'default', slots: 1 },
      });
      expect(out).toEqual({
        environment: { image: { cpu: 'img' } },
        resources: { resource_pool: 'default', slots: 1 },
      });
    });

    it('keeps a description from the form or a template', () => {
      expect(toShellConfig({ description: 'debug box' }).description).toBe('debug box');
    });
  });

  describe('templateFromConfig', () => {
    it('drops description, nulls and secrets from a full snapshot', () => {
      const out = templateFromConfig(mergedShellConfig);
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
    it('reads name, pool and slots, ignoring a generated name', () => {
      expect(formFieldsFromConfig(mergedShellConfig)).toEqual({
        name: '',
        pool: 'gpu',
        slots: 2,
      });
      expect(formFieldsFromConfig({ description: 'mine' }).name).toBe('mine');
    });

    it('applies the form on top of a base config without touching other keys', () => {
      const out = configFromForm(sanitizeConfig(mergedShellConfig), {
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
