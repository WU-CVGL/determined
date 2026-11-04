import { cloneDeep, isEqual, isNil, isPlainObject } from 'lodash';

import { RawJson } from 'types';

/** The services a shell can run besides sshd, which every shell runs: config.services.<name>. */
export const SHELL_SERVICES = ['jupyter', 'vscode'] as const;

export type ShellService = (typeof SHELL_SERVICES)[number];

/**
 * Per service of SHELL_SERVICES, whether it runs: true sends an entry that runs it, false one that
 * sets `enabled: false`. A service without a key gets no entry.
 */
export type ShellServiceChoices = Partial<Record<ShellService, boolean>>;

/** The simple launch form's fields, as the launch helpers take them. */
export interface NtscLaunchOptions {
  name?: string;
  pool?: string;
  /** The services to send. Undefined or empty sends no services: the master runs its defaults. */
  services?: ShellServiceChoices;
  slots?: number;
  template?: string;
  workspaceId?: number;
}

export interface LaunchFormFields {
  name?: string;
  pool?: string;
  /**
   * The services of a config, or to set in one; a service without a key keeps the config's entry.
   */
  services?: ShellServiceChoices;
  slots?: number;
}

/** The master names a shell "Shell (<pet name>)" when no description is given. */
const AUTO_DESCRIPTION = /^Shell \([a-z]+(-[a-z]+)*\)$/;

/** Environment variable names that probably hold a credential. */
const SECRET_ENV_NAME = /TOKEN|SECRET|PASSW|KEY|AUTH|CRED/i;

const asObject = (value: unknown): RawJson | undefined =>
  isPlainObject(value) ? (value as RawJson) : undefined;

/**
 * Whether a config's entry for a service runs it: a service runs unless its entry sets
 * `enabled: false`. A null entry is the master's empty one. A service without an entry does not
 * run: the master's defaults are already in the configs it returns.
 */
const serviceRuns = (entry: unknown): boolean =>
  entry !== undefined && asObject(entry)?.enabled !== false;

/** Sets a service's entry to run the service or not, keeping its other options. */
const serviceEntry = (entry: unknown, runs: boolean): RawJson => {
  const options = { ...asObject(entry) };
  if (runs) delete options.enabled;
  else options.enabled = false;
  return options;
};

export const isAutoDescription = (description: unknown): boolean =>
  typeof description === 'string' && AUTO_DESCRIPTION.test(description);

/** The services of SHELL_SERVICES that have a choice, with it. */
const serviceChoices = (choices: ShellServiceChoices = {}): [ShellService, boolean][] =>
  SHELL_SERVICES.flatMap((name) => {
    const runs = choices[name];
    return runs === undefined ? [] : [[name, runs]];
  });

/**
 * The config the simple form sends: a description, the resources and an entry per service with a
 * choice: `{}` for one to run, `{enabled: false}` for one not to.
 */
export const simpleLaunchConfig = (options: NtscLaunchOptions): RawJson => {
  const services = serviceChoices(options.services);
  return {
    description: options.name === '' ? undefined : options.name,
    resources: {
      resource_pool: options.pool === '' ? undefined : options.pool,
      slots: options.slots,
    },
    ...(services.length === 0
      ? {}
      : {
          services: Object.fromEntries(
            services.map(([name, runs]) => [name, serviceEntry(undefined, runs)]),
          ),
        }),
  };
};

/**
 * Prepares a merged config (from the master) for another launch.
 * - entrypoint: the master sets it on every launch (for a shell it holds the
 *   sshd command line with a random port).
 * - an auto-generated description, so the next launch gets a new pet name.
 * - environment.registry_auth: it can hold registry credentials.
 * - resources.priority: the master filled in the pool's default; dropping it
 *   lets the default of the chosen pool apply again.
 * Keys are deleted rather than set to null, so the master's defaults apply.
 * Everything else (image, bind mounts, environment variables, ...) is kept.
 */
export const sanitizeConfig = (config: RawJson): RawJson => {
  const out = cloneDeep(config);
  delete out.entrypoint;
  if (isAutoDescription(out.description)) delete out.description;
  const environment = asObject(out.environment);
  if (environment) delete environment.registry_auth;
  const resources = asObject(out.resources);
  if (resources) delete resources.priority;
  return out;
};

const dropNulls = (value: RawJson): RawJson => {
  const out: RawJson = {};
  Object.entries(value).forEach(([key, item]) => {
    if (item === null || item === undefined) return;
    const object = asObject(item);
    out[key] = object ? dropNulls(object) : item;
  });
  return out;
};

/**
 * Keeps only the keys of `config` whose values differ from `defaults`.
 * Objects are compared key by key, everything else (including arrays) as a whole.
 *
 * A null clears a setting, so it is kept where the default sets one. Where the
 * default is null or missing too, it changes nothing and is dropped, as are
 * the nulls inside an object that has no default object to compare with.
 */
export const minimalDiff = (config: RawJson, defaults: RawJson): RawJson => {
  const out: RawJson = {};
  Object.entries(config).forEach(([key, value]) => {
    const base = defaults[key];
    if (value === undefined || isEqual(value, base)) return;
    if (value === null && isNil(base)) return;
    const valueObject = asObject(value);
    const baseObject = asObject(base);
    if (valueObject && baseObject) {
      const diff = minimalDiff(valueObject, baseObject);
      if (Object.keys(diff).length !== 0) out[key] = diff;
      return;
    }
    out[key] = valueObject ? dropNulls(valueObject) : value;
  });
  return out;
};

/**
 * Builds a template from a launch config.
 *
 * It applies sanitizeConfig and always drops the description (a template
 * should not name every task launched from it). A null in a template clears
 * the cluster default, like a null in a launch config does.
 *
 * Only the settings that differ from `defaults`, the cluster defaults for the
 * same workspace, pool and slots, are kept (see minimalDiff), so the template
 * keeps following later changes to the defaults. A null that clears a default
 * is one of those settings. The defaults are required: without them there is
 * no telling an unset setting from a cleared one.
 *
 * A service's entry with `enabled: false` is kept even where the defaults do
 * not run the service, so that the template still turns it off if the cluster
 * runs it later: a missing entry follows the defaults.
 *
 * The resource pool and slots are always kept: the master ignores template
 * resources for shells, and the launch form copies them into its fields when
 * the template is picked.
 */
export const templateFromConfig = (config: RawJson, defaults: RawJson): RawJson => {
  const clean = sanitizeConfig(config);
  delete clean.description;
  const cleanDefaults = sanitizeConfig(defaults);
  delete cleanDefaults.description;
  const out = minimalDiff(clean, cleanDefaults);
  const resources = asObject(clean.resources);
  if (resources) {
    const kept: RawJson = {};
    if (!isNil(resources.resource_pool)) kept.resource_pool = resources.resource_pool;
    if (!isNil(resources.slots)) kept.slots = resources.slots;
    if (Object.keys(kept).length !== 0) out.resources = { ...asObject(out.resources), ...kept };
  }
  return out;
};

/** Reads the simple form's fields out of a config: whether each service with an entry runs. */
export const formFieldsFromConfig = (config: RawJson): LaunchFormFields => {
  const resources = asObject(config.resources);
  const description = config.description;
  const services = asObject(config.services);
  return {
    name: typeof description === 'string' && !isAutoDescription(description) ? description : '',
    pool: typeof resources?.resource_pool === 'string' ? resources.resource_pool : undefined,
    services: Object.fromEntries(
      SHELL_SERVICES.filter((name) => services?.[name] !== undefined).map((name) => [
        name,
        serviceRuns(services?.[name]),
      ]),
    ),
    slots: typeof resources?.slots === 'number' ? resources.slots : undefined,
  };
};

/** Applies the simple form's fields on top of a base config. */
export const configFromForm = (base: RawJson, fields: LaunchFormFields): RawJson => {
  const out = cloneDeep(base);
  if (fields.name) out.description = fields.name;
  else delete out.description;

  const resources: RawJson = { ...asObject(out.resources) };
  if (fields.pool) resources.resource_pool = fields.pool;
  else delete resources.resource_pool;
  if (fields.slots !== undefined && fields.slots !== null) resources.slots = fields.slots;
  else delete resources.slots;
  out.resources = resources;

  // The services with a choice follow the form, each keeping its other options; the others stay.
  const choices = serviceChoices(fields.services);
  if (choices.length !== 0) {
    const services: RawJson = { ...asObject(out.services) };
    choices.forEach(([name, runs]) => {
      services[name] = serviceEntry(services[name], runs);
    });
    out.services = services;
  }
  return out;
};

/** Pool and slots a template sets, if any. */
export const templateResources = (config?: RawJson): LaunchFormFields => {
  const resources = asObject(config?.resources);
  return {
    pool: typeof resources?.resource_pool === 'string' ? resources.resource_pool : undefined,
    slots: typeof resources?.slots === 'number' ? resources.slots : undefined,
  };
};

const ENV_RUNTIMES = ['cpu', 'cuda', 'rocm', 'gpu'];

const envVarName = (entry: unknown): string | undefined =>
  typeof entry === 'string' ? entry.split('=')[0] : undefined;

/** The lists of a Kubernetes pod spec whose containers have env entries. */
const POD_SPEC_CONTAINER_LISTS = ['containers', 'initContainers'];

/** The containers and init containers of environment.pod_spec (Kubernetes only). */
const podSpecContainers = (config: RawJson): RawJson[] => {
  const spec = asObject(asObject(asObject(config.environment)?.pod_spec)?.spec);
  return POD_SPEC_CONTAINER_LISTS.flatMap((key) => {
    const containers: unknown = spec?.[key];
    return Array.isArray(containers) ? containers : [];
  })
    .map(asObject)
    .filter((container): container is RawJson => container !== undefined);
};

/**
 * The name of a pod spec env entry ({ name, value }) whose name looks like a
 * credential and whose value is written out. An entry that only refers to a
 * value elsewhere (valueFrom) is left alone.
 */
const sensitivePodEnvName = (entry: unknown): string | undefined => {
  const item = asObject(entry);
  if (typeof item?.name !== 'string' || isNil(item.value)) return undefined;
  return SECRET_ENV_NAME.test(item.name) ? item.name : undefined;
};

/**
 * Lists the names of environment variables that look like credentials:
 * - environment_variables, either a list of NAME=VALUE strings or a map of
 *   such lists per runtime (cpu, cuda, rocm);
 * - the env entries with a value of the containers and init containers in
 *   environment.pod_spec (Kubernetes only).
 */
export const sensitiveEnvNames = (config: RawJson): string[] => {
  const variables = asObject(config.environment)?.environment_variables;
  const lists: unknown[][] = Array.isArray(variables)
    ? [variables]
    : ENV_RUNTIMES.map((runtime) => asObject(variables)?.[runtime]).filter(Array.isArray);
  const names = new Set<string>();
  lists.flat().forEach((entry) => {
    const name = envVarName(entry);
    if (name && SECRET_ENV_NAME.test(name)) names.add(name);
  });
  podSpecContainers(config).forEach((container) => {
    if (!Array.isArray(container.env)) return;
    container.env.forEach((entry: unknown) => {
      const name = sensitivePodEnvName(entry);
      if (name) names.add(name);
    });
  });
  return [...names].sort();
};

/**
 * Removes the environment variables and pod spec env entries that
 * sensitiveEnvNames lists and returns their names.
 */
export const redactSensitiveEnv = (config: RawJson): { config: RawJson; redacted: string[] } => {
  const redacted = sensitiveEnvNames(config);
  if (redacted.length === 0) return { config, redacted };

  const out = cloneDeep(config);
  const environment = asObject(out.environment);
  if (!environment) return { config: out, redacted };
  const keep = (list: unknown[]) =>
    list.filter((entry) => {
      const name = envVarName(entry);
      return !name || !SECRET_ENV_NAME.test(name);
    });
  const variables = environment.environment_variables;
  if (Array.isArray(variables)) {
    environment.environment_variables = keep(variables);
  } else {
    const perRuntime = asObject(variables);
    if (perRuntime) {
      ENV_RUNTIMES.forEach((runtime) => {
        if (Array.isArray(perRuntime[runtime])) perRuntime[runtime] = keep(perRuntime[runtime]);
      });
    }
  }
  podSpecContainers(out).forEach((container) => {
    if (Array.isArray(container.env)) {
      container.env = container.env.filter((entry: unknown) => !sensitivePodEnvName(entry));
    }
  });
  return { config: out, redacted };
};

/** JSON with object keys sorted, so equal configs give equal strings. */
export const stableStringify = (value: unknown): string => {
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(',')}]`;
  const object = asObject(value);
  if (object) {
    const entries = Object.keys(object)
      .sort()
      .filter((key) => object[key] !== undefined)
      .map((key) => `${JSON.stringify(key)}:${stableStringify(object[key])}`);
    return `{${entries.join(',')}}`;
  }
  return JSON.stringify(value) ?? 'null';
};
