-- Remove notebook_idle_type, which services.jupyter.idle_type replaces, from the stored configs:
-- - command_state.generic_command_spec: the config of every shell, command and TensorBoard
--   (Config.notebook_idle_type), and the NOTEBOOK_CONFIG copy of that config among its environment
--   variables (Base.ExtraEnvVars.NOTEBOOK_CONFIG), which nothing reads;
-- - templates.config;
-- - task_config_policies.invariant_config.
-- A kernels_or_terminals or kernel_connections value moves to services.jupyter.idle_type when the
-- jupyter entry has no idle_type:
-- - in a stored task's config, when it has a jupyter entry: a running or ended task keeps the
--   services it started with;
-- - in a template's config, when it has a non-empty services object, which only a shell template
--   has; a missing jupyter entry is added, and the next launch still adds each other default
--   service that the template does not name.
-- Otherwise, and for activity (the default), an empty value or any other value, the key is
-- dropped. Only rows with the key change, and the migration can run again.

UPDATE command_state
SET generic_command_spec = jsonb_set(
    generic_command_spec,
    '{Config,services,jupyter}',
    CASE jsonb_typeof(generic_command_spec->'Config'->'services'->'jupyter')
        WHEN 'object' THEN generic_command_spec->'Config'->'services'->'jupyter'
        ELSE '{}'::jsonb
    END || jsonb_build_object('idle_type', generic_command_spec->'Config'->'notebook_idle_type')
)
WHERE generic_command_spec->'Config'->>'notebook_idle_type'
    IN ('kernels_or_terminals', 'kernel_connections')
    AND jsonb_typeof(generic_command_spec->'Config'->'services'->'jupyter') IN ('object', 'null')
    AND coalesce(generic_command_spec->'Config'->'services'->'jupyter'->>'idle_type', '') = '';

UPDATE command_state
SET generic_command_spec = generic_command_spec
    #- '{Config,notebook_idle_type}'
    #- '{Base,ExtraEnvVars,NOTEBOOK_CONFIG}'
WHERE jsonb_exists(generic_command_spec->'Config', 'notebook_idle_type')
    OR jsonb_exists(generic_command_spec->'Base'->'ExtraEnvVars', 'NOTEBOOK_CONFIG');

UPDATE templates
SET config = jsonb_set(
    config,
    '{services,jupyter}',
    CASE jsonb_typeof(config->'services'->'jupyter')
        WHEN 'object' THEN config->'services'->'jupyter'
        ELSE '{}'::jsonb
    END || jsonb_build_object('idle_type', config->'notebook_idle_type')
)
WHERE config->>'notebook_idle_type' IN ('kernels_or_terminals', 'kernel_connections')
    AND jsonb_typeof(config->'services') = 'object'
    AND config->'services' <> '{}'::jsonb
    AND coalesce(jsonb_typeof(config->'services'->'jupyter'), 'null') IN ('object', 'null')
    AND coalesce(config->'services'->'jupyter'->>'idle_type', '') = '';

UPDATE templates
SET config = config - 'notebook_idle_type'
WHERE jsonb_typeof(config) = 'object' AND jsonb_exists(config, 'notebook_idle_type');

-- The master does not apply an NTSC invariant config, so its value has nothing to carry.
UPDATE task_config_policies
SET invariant_config = invariant_config - 'notebook_idle_type'
WHERE jsonb_typeof(invariant_config) = 'object'
    AND jsonb_exists(invariant_config, 'notebook_idle_type');
