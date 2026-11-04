import Alert from 'hew/Alert';
import Button from 'hew/Button';
import Checkbox from 'hew/Checkbox';
import Form, { FormInstance } from 'hew/Form';
import Input from 'hew/Input';
import InputNumber from 'hew/InputNumber';
import { Modal, useModal } from 'hew/Modal';
import Row from 'hew/Row';
import Select, { Option, SelectValue } from 'hew/Select';
import Spinner from 'hew/Spinner';
import { Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import { number, string, undefined as undefinedType, union } from 'io-ts';
import yaml from 'js-yaml';
import { isPlainObject } from 'lodash';
import React, { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';

import Link from 'components/Link';
import { resourcePoolOption } from 'components/ResourcePoolOption';
import StartFromSelect, { StartFrom, templateNameFromStartFrom } from 'components/StartFromSelect';
import useFeature from 'hooks/useFeature';
import usePermissions from 'hooks/usePermissions';
import { SettingsConfig, useSettings } from 'hooks/useSettings';
import TemplateCreateModalComponent from 'pages/Templates/TemplateCreateModal';
import { paths } from 'routes/utils';
import clusterStore from 'stores/cluster';
import workspaceStore from 'stores/workspaces';
import { CommandResponse, CommandTask, RawJson, ResourcePool, Workspace } from 'types';
import { isUsablePool } from 'utils/cluster';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import {
  configFromForm,
  formFieldsFromConfig,
  NtscLaunchOptions,
  sanitizeConfig,
  SHELL_SERVICE_LABELS,
  SHELL_SERVICES,
  ShellService,
  ShellServiceChoices,
  templateFromConfig,
  templateResources,
} from 'utils/ntscConfig';
import { useObservable } from 'utils/observable';
import { launchShell, PREVIEW_ERROR, previewShell } from 'utils/shell';

const DEFAULT_SLOT_COUNT = 1;
const BASE_FORM_ID = 'ntsc-launch-form';

/** The user's last-used form values. The services are not among them: the master's defaults are. */
type LaunchFormSettings = Omit<NtscLaunchOptions, 'services'>;

/**
 * The simple form's values. `source` is the "Start from" picker's value; `services` holds a
 * checkbox per service of SHELL_SERVICES, unset until the master's preview of the start-from
 * fills them with what it would run.
 */
interface LaunchFormValues extends LaunchFormSettings {
  services?: Partial<Record<string, boolean>>;
  source?: string;
}

/**
 * The services to send: a checked one runs, an unchecked one that the master offered is turned
 * off, and one it did not offer is left out. Undefined while the checkboxes are unset: the master
 * then decides.
 */
const checkedServices = (
  values: LaunchFormValues,
  offered: readonly ShellService[],
): ShellServiceChoices | undefined => {
  if (values.services === undefined) return undefined;
  const choices: ShellServiceChoices = {};
  SHELL_SERVICES.forEach((name) => {
    if (values.services?.[name]) choices[name] = true;
    else if (offered.includes(name)) choices[name] = false;
  });
  return choices;
};

const serviceChecks = (choices: ShellServiceChoices = {}): Record<string, boolean> =>
  Object.fromEntries(SHELL_SERVICES.map((name) => [name, choices[name] ?? false]));

/** The services that a config has an entry for: the ones the master offers for it. */
const offeredServices = (choices: ShellServiceChoices = {}): ShellService[] =>
  SHELL_SERVICES.filter((name) => choices[name] !== undefined);

/**
 * A preview of the start-from's services: the start-from it was sent for, its number, the
 * checkboxes as they were when the start-from's first preview was sent, and whether it is on its
 * way, succeeded or failed with error.
 */
interface ServicesPreview {
  baseConfig?: RawJson;
  error?: string;
  sent?: LaunchFormValues['services'];
  seq: number;
  source?: string;
  status: 'pending' | 'success' | 'error';
}

/** The user's last-used form values, kept on the server. */
const settingsConfig: SettingsConfig<LaunchFormSettings> = {
  settings: {
    name: {
      defaultValue: '',
      skipUrlEncoding: true,
      storageKey: 'name',
      type: union([string, undefinedType]),
    },
    pool: {
      defaultValue: '',
      skipUrlEncoding: true,
      storageKey: 'pool',
      type: union([string, undefinedType]),
    },
    slots: {
      defaultValue: DEFAULT_SLOT_COUNT,
      skipUrlEncoding: true,
      storageKey: 'slots',
      type: union([number, undefinedType]),
    },
    template: {
      defaultValue: undefined,
      skipUrlEncoding: true,
      storageKey: 'template',
      type: union([string, undefinedType]),
    },
    workspaceId: {
      defaultValue: undefined,
      skipUrlEncoding: true,
      storageKey: 'workspaceId',
      type: union([number, undefinedType]),
    },
  },
  storagePath: 'shell-launch',
};

const copy = {
  configRequired: 'Shell config required',
  docsLabel: 'Read about shell settings',
  title: 'Launch Interactive Task',
};

const settingsFromForm = (values: LaunchFormValues): LaunchFormSettings => ({
  name: values.name,
  pool: values.pool,
  slots: values.slots,
  template: templateNameFromStartFrom(values.source),
  workspaceId: values.workspaceId,
});

export interface Props {
  /** "Launch Again": start from this task's config. */
  initialTask?: CommandTask;
  /** Called with the new shell after a successful launch, to show how to connect. */
  onShellLaunched?: (response: CommandResponse) => void;
  workspace?: Workspace;
}

const CodeEditor = React.lazy(() => import('hew/CodeEditor'));

/**
 * The shell launch modal: a simple form (workspace, "Start from", name, pool,
 * slots, services) and a full-config YAML mode, previewed through the master's
 * shell preview with what a launch of the simple form sends.
 *
 * Start from a template: the template name is sent with the simple fields.
 * Start from a config (a recent task or this browser's history): the config
 * is the base and the simple fields are applied on top of it; no template is
 * sent. A recent task's config is fetched when it is picked; Launch and the
 * full config stay disabled until it arrives, and until the master's preview
 * of the start-from's services answers.
 */
const NtscLaunchModalComponent: React.FC<Props> = ({
  initialTask,
  onShellLaunched,
  workspace,
}: Props) => {
  const idPrefix = useId();
  const [showFullConfig, setShowFullConfig] = useState(false);
  const [config, setConfig] = useState<Loadable<string>>(NotLoaded);
  const [configError, setConfigError] = useState<string>();
  const [fullConfigFormInvalid, setFullConfigFormInvalid] = useState(true);
  const [baseConfig, setBaseConfig] = useState<RawJson>();
  const [redactedEnv, setRedactedEnv] = useState<string[]>([]);
  const [templateDraft, setTemplateDraft] = useState<string>();
  const [autoSelectStartFrom, setAutoSelectStartFrom] = useState(true);
  // The recent task whose config "Start from" is fetching. Until it arrives,
  // baseConfig still belongs to the previous pick (or to none), so Launch and
  // the full config wait for it. "Launch Again" starts fetching right away.
  const [startFromLoadingTask, setStartFromLoadingTask] = useState<string | undefined>(
    initialTask?.id,
  );
  const startFromLoading = startFromLoadingTask !== undefined;
  const [form] = Form.useForm<LaunchFormValues>();
  const [fullConfigForm] = Form.useForm();
  const { canCreateTemplateWorkspace, canCreateWorkspaceNSC } = usePermissions();
  const templatesOn = useFeature().isOn('task_templates');
  const TemplateCreateModal = useModal(TemplateCreateModalComponent);
  const openTemplateCreate = TemplateCreateModal.open;
  const workspaces = Loadable.getOrElse([], useObservable(workspaceStore.workspaces)).filter(
    (workspace) => canCreateWorkspaceNSC({ workspace }),
  );
  const [currentWorkspace, setCurrentWorkspace] = useState<Workspace | undefined>(
    workspace ?? workspaces.at(0),
  );
  const validateFullConfigForm = useCallback(() => {
    const fields = fullConfigForm.getFieldsError();
    const hasError = fields.some((f) => f.errors.length) || !currentWorkspace;
    setFullConfigFormInvalid(hasError);
  }, [currentWorkspace, fullConfigForm]);

  const { settings: defaults, updateSettings: saveDefaults } =
    useSettings<LaunchFormSettings>(settingsConfig);

  const handleModalClose = useCallback(() => {
    const fields: LaunchFormValues = form.getFieldsValue(true);
    saveDefaults(settingsFromForm(fields));
  }, [form, saveDefaults]);

  // The services that the start-from's config or preview has an entry for.
  const offered = useRef<ShellService[]>([]);

  /** The launch or preview options for the simple form's current values. */
  const simpleOptions = useCallback(
    (fields: LaunchFormValues) =>
      baseConfig
        ? {
            config: configFromForm(baseConfig, {
              ...fields,
              services: checkedServices(fields, offered.current),
            }),
            workspaceId: fields.workspaceId,
          }
        : {
            name: fields.name,
            pool: fields.pool,
            services: checkedServices(fields, offered.current),
            slots: fields.slots,
            template: templateNameFromStartFrom(fields.source),
            workspaceId: fields.workspaceId,
          },
    [baseConfig],
  );

  // The services that the start-from runs, previewed through the master without the checkboxes
  // each time the start-from changes, once the simple form shows it: the master's defaults, and
  // what a template or a config turns off. A new template or the defaults clear the checkboxes
  // first; a config fills them from its entries. The checkboxes follow the latest preview, except a
  // box changed since the start-from's first preview was sent, also while a refusal showed. Launch and the full config wait for a successful preview
  // of the current start-from: a refused one shows its message with Retry, keeps them disabled, and
  // is sent again when the pool or the workspace changes. Showing the simple form again after the
  // full config previews nothing, so that the checkboxes keep their changes.
  const [simpleFormShown, setSimpleFormShown] = useState(false);
  const source = Form.useWatch('source', form);
  const pool = Form.useWatch('pool', form);
  const workspaceId = Form.useWatch('workspaceId', form);
  // The last services preview sent, numbered so that a late answer is dropped, and its result.
  // The ref is what the effect reads; the state renders it.
  const servicesPreview = useRef<ServicesPreview>({ seq: 0, status: 'pending' });
  const [servicesResult, setServicesResult] = useState<ServicesPreview>(servicesPreview.current);
  const [servicesRetry, setServicesRetry] = useState(0);
  const setServicesPreview = useCallback((preview: ServicesPreview) => {
    servicesPreview.current = preview;
    setServicesResult(preview);
  }, []);
  const servicesConfirmed =
    servicesResult.status === 'success' &&
    servicesResult.source === source &&
    servicesResult.baseConfig === baseConfig;
  useEffect(() => {
    if (!simpleFormShown || startFromLoading) return;
    const last = servicesPreview.current;
    const sameStart = last.seq !== 0 && last.source === source && last.baseConfig === baseConfig;
    if (last.status !== 'error' && sameStart) return;
    const seq = last.seq + 1;
    if (!sameStart && !baseConfig) {
      // The checkboxes were filled for another start-from.
      offered.current = [];
      form.resetFields(SHELL_SERVICES.map((name) => ['services', name]));
    }
    const fields: LaunchFormValues = form.getFieldsValue(true);
    // A retry, or a pool or workspace change after a refusal, keeps the boxes changed since.
    const sent = sameStart ? last.sent : fields.services;
    setServicesPreview({ baseConfig, sent, seq, source, status: 'pending' });
    previewShell(simpleOptions({ ...fields, services: undefined, source }))
      .then((previewed) => {
        if (seq !== servicesPreview.current.seq) return;
        const choices = formFieldsFromConfig(previewed).services;
        offered.current = offeredServices(choices);
        const checks = serviceChecks(choices);
        const current: LaunchFormValues['services'] = form.getFieldValue('services');
        form.setFieldsValue({
          services: Object.fromEntries(
            SHELL_SERVICES.map((name) => [
              name,
              current?.[name] === sent?.[name] ? checks[name] : current?.[name],
            ]),
          ),
        });
        setServicesPreview({ baseConfig, sent, seq, source, status: 'success' });
      })
      .catch((e) => {
        if (seq !== servicesPreview.current.seq) return;
        setServicesPreview({
          baseConfig,
          error: e instanceof Error ? e.message : PREVIEW_ERROR,
          sent,
          seq,
          source,
          status: 'error',
        });
      });
  }, [
    baseConfig,
    form,
    pool,
    servicesRetry,
    setServicesPreview,
    simpleFormShown,
    simpleOptions,
    source,
    startFromLoading,
    workspaceId,
  ]);

  const handleServicesRetry = useCallback(() => setServicesRetry((count) => count + 1), []);

  // Previewed each time the full config opens, numbered so that a late answer is dropped.
  const configSeq = useRef(0);
  const fetchConfig = useCallback(async () => {
    const seq = ++configSeq.current;
    setConfig(NotLoaded);
    setConfigError(undefined);

    const fields: LaunchFormValues = form.getFieldsValue(true);
    try {
      const newConfig = await previewShell(simpleOptions(fields));
      if (seq === configSeq.current) setConfig(Loaded(yaml.dump(newConfig)));
    } catch (e) {
      if (seq === configSeq.current) setConfigError(e instanceof Error ? e.message : PREVIEW_ERROR);
    }
  }, [form, simpleOptions]);

  const handleSecondary = useCallback(() => {
    if (showFullConfig) {
      configSeq.current++;
      setFullConfigFormInvalid(false);
    } else if (servicesConfirmed) fetchConfig();
    else return;
    setShowFullConfig((show) => !show);
  }, [fetchConfig, servicesConfirmed, showFullConfig]);

  const launch = useCallback(
    async (options: NtscLaunchOptions & { config?: RawJson }) => {
      const response = await launchShell(options);
      onShellLaunched?.(response);
    },
    [onShellLaunched],
  );

  const handleSubmit = useCallback(async () => {
    const fields: LaunchFormValues = form.getFieldsValue(true);
    saveDefaults(settingsFromForm(fields));
    if (showFullConfig) {
      const values = await fullConfigForm.validateFields();
      const usableConfig = Loadable.isLoaded(config) ? config.data : '';

      if (values) {
        await launch({
          config: yaml.load(usableConfig) as RawJson,
          workspaceId: values.workspaceId,
        });
      }
    } else if (servicesConfirmed) {
      const values = await form.validateFields();
      if (values) await launch(simpleOptions(fields));
    }
  }, [
    config,
    fullConfigForm,
    form,
    launch,
    saveDefaults,
    servicesConfirmed,
    showFullConfig,
    simpleOptions,
  ]);

  const handleConfigChange = useCallback(
    (config: string) => {
      validateFullConfigForm();
      setConfig(Loaded(config));
      setConfigError(undefined);
    },
    [validateFullConfigForm],
  );

  const handleStartFromAutoSelected = useCallback(() => setAutoSelectStartFrom(false), []);

  const handleStartFrom = useCallback(
    (start: StartFrom) => {
      if (start.kind === 'config') {
        const base = sanitizeConfig(start.config);
        const fields = formFieldsFromConfig(base);
        setBaseConfig(base);
        setRedactedEnv(start.redactedEnv);
        offered.current = offeredServices(fields.services);
        form.setFieldsValue({
          name: fields.name,
          pool: fields.pool,
          services: serviceChecks(fields.services),
          slots: fields.slots,
        });
        if (!workspace) {
          const sourceWorkspace = workspaces.find((w) => w.id === start.workspaceId);
          if (sourceWorkspace) setCurrentWorkspace(sourceWorkspace);
        }
        return;
      }
      setBaseConfig(undefined);
      setRedactedEnv([]);
      if (start.kind === 'template') {
        // The master ignores a template's resources for shells, so they are
        // copied into the form, which always sends them.
        const { pool, slots } = templateResources(start.template.config);
        if (pool !== undefined) form.setFieldValue('pool', pool);
        if (slots !== undefined) form.setFieldValue('slots', slots);
      }
    },
    [form, workspace, workspaces],
  );

  const canSaveTemplate =
    templatesOn &&
    !!currentWorkspace &&
    canCreateTemplateWorkspace({ workspace: { id: currentWorkspace.id } });

  const handleSaveAsTemplate = useCallback(async () => {
    let parsed: unknown;
    try {
      parsed = yaml.load(Loadable.isLoaded(config) ? config.data : '');
    } catch (e) {
      handleError(e, {
        level: ErrorLevel.Error,
        publicSubject: 'Fix the YAML before saving it as a template.',
        silent: false,
        type: ErrorType.Input,
      });
      return;
    }
    if (!isPlainObject(parsed)) {
      handleError(new Error('The config is empty.'), {
        level: ErrorLevel.Error,
        publicSubject: 'Fix the YAML before saving it as a template.',
        silent: false,
        type: ErrorType.Input,
      });
      return;
    }
    const parsedConfig = parsed as RawJson;
    // The cluster defaults for the same workspace, pool and slots, so that the
    // template only keeps what differs from them. Without them a null that
    // clears a default cannot be told from an unset setting, so no template is
    // started and the config stays here to try again.
    let defaultsConfig: RawJson;
    try {
      const resources = templateResources(parsedConfig);
      defaultsConfig = await previewShell({
        pool: resources.pool,
        slots: resources.slots,
        workspaceId: currentWorkspace?.id,
      });
    } catch (e) {
      handleError(e, {
        level: ErrorLevel.Error,
        publicMessage: e instanceof Error && e.message !== PREVIEW_ERROR ? e.message : undefined,
        publicSubject: 'Unable to load the cluster defaults. Try Save as Template again.',
        silent: false,
        type: ErrorType.Server,
      });
      return;
    }
    const header = '# Only the settings that differ from the cluster defaults are kept.\n';
    setTemplateDraft(header + yaml.dump(templateFromConfig(parsedConfig, defaultsConfig)));
    openTemplateCreate();
  }, [config, currentWorkspace?.id, openTemplateCreate]);

  useEffect(validateFullConfigForm, [currentWorkspace, validateFullConfigForm]);

  useEffect(() => workspaceStore.fetch(), []);

  return (
    <Modal
      cancel
      footerLink={
        showFullConfig ? (
          <Link
            external
            path={paths.docs('/architecture/introduction.html#interactive-job-configuration')}
            popout>
            {copy.docsLabel}
          </Link>
        ) : undefined
      }
      size={showFullConfig ? 'large' : 'small'}
      submit={{
        disabled: showFullConfig
          ? fullConfigFormInvalid
          : !currentWorkspace?.id || startFromLoading || !servicesConfirmed,
        form: idPrefix + (showFullConfig ? '-full-' : '-simple-') + BASE_FORM_ID,
        handleError,
        handler: handleSubmit,
        text: 'Launch',
      }}
      title={copy.title}
      onClose={handleModalClose}>
      {showFullConfig ? (
        <FullConfig
          config={config}
          configError={configError}
          configRequired={copy.configRequired}
          currentWorkspace={currentWorkspace}
          form={fullConfigForm}
          formId={idPrefix + '-full-' + BASE_FORM_ID}
          lockedWorkspace={!!workspace}
          setWorkspace={setCurrentWorkspace}
          workspaces={workspaces}
          onChange={handleConfigChange}
        />
      ) : (
        <LaunchForm
          autoSelectStartFrom={autoSelectStartFrom}
          currentWorkspace={currentWorkspace}
          defaults={defaults}
          form={form}
          formId={idPrefix + '-simple-' + BASE_FORM_ID}
          initialTask={initialTask}
          lockedWorkspace={!!workspace}
          servicesError={servicesResult.status === 'error' ? servicesResult.error : undefined}
          setWorkspace={setCurrentWorkspace}
          startFromLoadingTask={startFromLoadingTask}
          workspaces={workspaces}
          onServicesRetry={handleServicesRetry}
          onShown={setSimpleFormShown}
          onStartFrom={handleStartFrom}
          onStartFromAutoSelected={handleStartFromAutoSelected}
          onStartFromLoadingTask={setStartFromLoadingTask}
        />
      )}
      {redactedEnv.length > 0 && (
        <Alert
          description={`Add them back in the full config if this task needs them: ${redactedEnv.join(', ')}.`}
          message="Environment variables that looked like credentials were not saved in this browser."
          showIcon
          type="warning"
        />
      )}
      <Row>
        <Button
          disabled={startFromLoading || (!showFullConfig && !servicesConfirmed)}
          onClick={handleSecondary}>
          {showFullConfig ? 'Show Simple Config' : 'Show Full Config'}
        </Button>
        {showFullConfig && canSaveTemplate && (
          <Button disabled={!Loadable.isLoaded(config)} onClick={handleSaveAsTemplate}>
            Save as Template
          </Button>
        )}
      </Row>
      {templateDraft !== undefined && (
        <TemplateCreateModal.Component
          initialConfig={templateDraft}
          initialWorkspaceId={currentWorkspace?.id}
        />
      )}
    </Modal>
  );
};

export default NtscLaunchModalComponent;

interface FullConfigProps {
  config: Loadable<string>;
  configError?: string;
  configRequired: string;
  currentWorkspace?: Workspace;
  form: FormInstance;
  formId: string;
  lockedWorkspace: boolean;
  onChange?: (config: string) => void;
  setWorkspace: (arg0: Workspace | undefined) => void;
  workspaces: Workspace[];
}

const FullConfig: React.FC<FullConfigProps> = ({
  config,
  configError,
  configRequired,
  currentWorkspace,
  form,
  formId,
  lockedWorkspace,
  onChange,
  setWorkspace,
  workspaces,
}: FullConfigProps) => {
  const usableConfig = useMemo(() => (Loadable.isLoaded(config) ? config.data : ''), [config]);
  const [field, setField] = useState([
    { name: 'config', value: usableConfig },
    { name: 'workspaceId', value: currentWorkspace ? currentWorkspace.id : workspaces.at(0)?.id },
  ]);

  const handleConfigChange = useCallback(
    (_: unknown, allFields: unknown) => {
      if (!Array.isArray(allFields) || allFields.length === 0) return;
      try {
        const configString = allFields.find((field) => field.name[0] === 'config').value;
        onChange?.(configString);
      } catch (e) {
        handleError(e);
      }
    },
    [onChange],
  );

  useEffect(() => {
    setField((curField) => [
      ...curField.filter((f) => f.name[0] === 'workspaceId'),
      { name: 'config', value: usableConfig },
    ]);
  }, [usableConfig]);

  useEffect(() => {
    form.setFieldValue(
      'workspaceId',
      currentWorkspace ? currentWorkspace.id : workspaces.at(0)?.id,
    );
  }, [currentWorkspace, form, workspaces]);
  useEffect(() => {
    form.setFieldValue('config', usableConfig);
  }, [usableConfig, form]);

  const onSelectWorkspace = (workspaceId?: SelectValue) => {
    const selected = workspaces.find((w) => workspaceId && w.id === workspaceId);
    setWorkspace(selected);
  };

  return (
    <Form fields={field} form={form} id={formId} onFieldsChange={handleConfigChange}>
      <React.Suspense fallback={<Spinner spinning tip="Loading text editor..." />}>
        <Form.Item
          initialValue={currentWorkspace?.id}
          label="Workspace"
          name="workspaceId"
          rules={[{ message: 'Workspace is required', required: true, type: 'number' }]}>
          <Select
            allowClear
            disabled={lockedWorkspace}
            placeholder="Workspace (required)"
            onChange={onSelectWorkspace}>
            {workspaces.map((workspace: Workspace) => (
              <Option key={workspace.id} value={workspace.id}>
                {workspace.name}
              </Option>
            ))}
          </Select>
        </Form.Item>
        <Form.Item
          initialValue={usableConfig}
          name="config"
          rules={[
            { message: configRequired, required: true },
            {
              validator: (_rule, value) => {
                try {
                  yaml.load(value);
                  return Promise.resolve();
                } catch (err: unknown) {
                  return Promise.reject(
                    new Error(
                      `Invalid YAML on line ${(err as { mark: { line: string } }).mark.line}.`,
                    ),
                  );
                }
              },
            },
          ]}>
          <CodeEditor
            file={config}
            files={[{ key: 'config.yaml' }]}
            height="40vh"
            onError={handleError}
          />
        </Form.Item>
      </React.Suspense>
      {configError && <Alert message={configError} type="error" />}
    </Form>
  );
};

interface LaunchFormProps {
  autoSelectStartFrom: boolean;
  currentWorkspace?: Workspace;
  defaults: LaunchFormSettings;
  form: FormInstance<LaunchFormValues>;
  formId: string;
  initialTask?: CommandTask;
  lockedWorkspace: boolean;
  onServicesRetry: () => void;
  /** Called with true once the form shows, and with false when it is replaced. */
  onShown: (shown: boolean) => void;
  onStartFrom: (start: StartFrom) => void;
  onStartFromAutoSelected: () => void;
  onStartFromLoadingTask: (taskId?: string) => void;
  /** The message of the refused preview of the services, if the last one was refused. */
  servicesError?: string;
  setWorkspace: (arg0: Workspace | undefined) => void;
  startFromLoadingTask?: string;
  workspaces: Workspace[];
}

const LaunchForm: React.FC<LaunchFormProps> = ({
  autoSelectStartFrom,
  currentWorkspace,
  defaults,
  form,
  formId,
  initialTask,
  lockedWorkspace,
  onServicesRetry,
  onShown,
  onStartFrom,
  onStartFromAutoSelected,
  onStartFromLoadingTask,
  servicesError,
  setWorkspace,
  startFromLoadingTask,
  workspaces,
}: LaunchFormProps) => {
  const selectedWorkspaceId = Form.useWatch('workspaceId', form);
  const templatesOn = useFeature().isOn('task_templates');

  useEffect(() => {
    onShown(true);
    return () => onShown(false);
  }, [onShown]);

  const resourcePools = useObservable(clusterStore.resourcePools);
  const boundResourcePoolsMap = useObservable(workspaceStore.boundResourcePoolsMap());

  const boundResourcePools: ResourcePool[] = useMemo(() => {
    if (!Loadable.isLoaded(resourcePools) || !selectedWorkspaceId) return [];
    return resourcePools.data.filter((rp) =>
      boundResourcePoolsMap.get(selectedWorkspaceId)?.includes(rp.name),
    );
  }, [resourcePools, boundResourcePoolsMap, selectedWorkspaceId]);

  const selectedPoolName = Form.useWatch('pool', form);

  const resourceInfo = useMemo(() => {
    const selectedPool = boundResourcePools.find((pool) => pool.name === selectedPoolName);
    if (!selectedPool) return { hasAux: false, hasCompute: false, maxSlots: 0 };

    /**
     * For static resource pools, the slots-per-agent comes through as -1,
     * meaning it is unknown how many we may have.
     */
    const hasAuxCapacity = selectedPool.auxContainerCapacityPerAgent > 0;
    const hasSlots = selectedPool.slotsAvailable > 0;
    const maxSlots = selectedPool.slotsPerAgent ?? 0;
    const hasSlotsPerAgent = maxSlots !== 0;
    const hasComputeCapacity = hasSlots || hasSlotsPerAgent;

    return {
      hasAux: hasAuxCapacity,
      hasCompute: hasComputeCapacity,
      maxSlots: maxSlots,
    };
  }, [selectedPoolName, boundResourcePools]);

  const allowedWorkspaceIds = useMemo(() => workspaces.map((w) => w.id), [workspaces]);

  useEffect(() => {
    if (!resourceInfo.hasCompute && resourceInfo.hasAux) form.setFieldValue('slots', 0);
    else if (resourceInfo.hasCompute) {
      const slots = form.getFieldValue('slots');
      if (slots == null) form.setFieldValue('slots', DEFAULT_SLOT_COUNT);
    }
  }, [resourceInfo, form]);

  useEffect(() => {
    selectedWorkspaceId && workspaceStore.fetchAvailableResourcePools(selectedWorkspaceId);
  }, [selectedWorkspaceId]);

  useEffect(() => {
    const fields = form.getFieldsValue(true);
    const firstUsablePool = boundResourcePools.find(isUsablePool)?.name;
    if (!fields?.pool && firstUsablePool) form.setFieldValue('pool', firstUsablePool);
  }, [boundResourcePools, form]);

  useEffect(() => {
    form.setFieldValue(
      'workspaceId',
      currentWorkspace ? currentWorkspace.id : workspaces.at(0)?.id,
    );
  }, [currentWorkspace, form, workspaces]);

  const onSelectWorkspace = (workspaceId?: SelectValue) => {
    const selected = workspaces.find((w) => workspaceId && w.id === workspaceId);
    setWorkspace(selected);
  };

  return (
    <Form form={form} id={formId}>
      <Form.Item
        initialValue={currentWorkspace?.id}
        label="Workspace"
        name="workspaceId"
        rules={[{ message: 'Workspace is required', required: true, type: 'number' }]}>
        <Select
          allowClear
          disabled={lockedWorkspace}
          placeholder="Workspace (required)"
          onChange={onSelectWorkspace}>
          {workspaces.map((workspace: Workspace) => (
            <Option key={workspace.id} value={workspace.id}>
              {workspace.name}
            </Option>
          ))}
        </Select>
      </Form.Item>
      <Form.Item
        extra={templatesOn && <Link path={paths.templates()}>Manage Templates</Link>}
        label="Start from"
        name="source">
        <StartFromSelect
          allowedWorkspaceIds={allowedWorkspaceIds}
          autoSelect={autoSelectStartFrom}
          defaultTemplate={defaults.template}
          initialTask={initialTask}
          loadingTaskId={startFromLoadingTask}
          lockedWorkspaceId={lockedWorkspace ? currentWorkspace?.id : undefined}
          onAutoSelected={onStartFromAutoSelected}
          onLoadingTaskChange={onStartFromLoadingTask}
          onResolve={onStartFrom}
        />
      </Form.Item>
      <Form.Item initialValue={defaults?.name} label="Name" name="name">
        <Input placeholder="Name (optional)" />
      </Form.Item>
      <Form.Item label="Resource Pool" name="pool">
        <Select allowClear disabled={!selectedWorkspaceId} placeholder="Pick the best option">
          {boundResourcePools.map(resourcePoolOption)}
        </Select>
      </Form.Item>
      <Form.Item
        hidden={!resourceInfo.hasCompute}
        initialValue={defaults?.slots}
        label="Slots"
        name="slots">
        <InputNumber
          max={resourceInfo.maxSlots === -1 ? Number.MAX_SAFE_INTEGER : resourceInfo.maxSlots}
          min={0}
        />
      </Form.Item>
      <Form.Item label="Services">
        <Row>
          {SHELL_SERVICES.map((name) => (
            <Form.Item key={name} name={['services', name]} noStyle valuePropName="checked">
              <Checkbox>{SHELL_SERVICE_LABELS[name]}</Checkbox>
            </Form.Item>
          ))}
        </Row>
      </Form.Item>
      {servicesError && (
        <Alert
          action={
            <Button size="small" onClick={onServicesRetry}>
              Retry
            </Button>
          }
          message={servicesError}
          type="error"
        />
      )}
    </Form>
  );
};
