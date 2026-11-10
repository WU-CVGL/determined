-- Remove tensorboard_args, which services.tensorboard.args replaces, from the stored configs:
-- - templates.config: in a template with a non-empty services object, which only a shell template
--   has, a non-empty list moves to services.tensorboard.args unless that entry sets args; a
--   missing tensorboard entry is added disabled, so that the template's shells do not run
--   TensorBoard while a viewing session launched from it takes the args. Otherwise it is dropped:
--   a template without services also serves commands, which refuse services, and a value that is
--   not a list never decoded.
-- - command_state.generic_command_spec: dropped from Config; only the TENSORBOARD tasks, which the
--   previous migration deleted, read it.
-- - task_config_policies.invariant_config: dropped; the master does not apply an NTSC invariant
--   config.
-- Only rows with the key change, and the migration can run again.
UPDATE templates
SET config = jsonb_set(
    config,
    '{services,tensorboard}',
    CASE jsonb_typeof(config->'services'->'tensorboard')
        WHEN 'object' THEN config->'services'->'tensorboard'
        ELSE '{"enabled": false}'::jsonb
    END || jsonb_build_object('args', config->'tensorboard_args')
)
WHERE jsonb_typeof(config) = 'object'
    AND jsonb_typeof(config->'tensorboard_args') = 'array'
    AND config->'tensorboard_args' <> '[]'::jsonb
    AND jsonb_typeof(config->'services') = 'object'
    AND config->'services' <> '{}'::jsonb
    AND coalesce(jsonb_typeof(config->'services'->'tensorboard'), 'null') IN ('object', 'null')
    AND NOT jsonb_exists(coalesce(config->'services'->'tensorboard', '{}'::jsonb), 'args');

UPDATE templates
SET config = config - 'tensorboard_args'
WHERE jsonb_typeof(config) = 'object' AND jsonb_exists(config, 'tensorboard_args');

UPDATE command_state
SET generic_command_spec = generic_command_spec #- '{Config,tensorboard_args}'
WHERE jsonb_exists(generic_command_spec->'Config', 'tensorboard_args');

UPDATE task_config_policies
SET invariant_config = invariant_config - 'tensorboard_args'
WHERE jsonb_typeof(invariant_config) = 'object'
    AND jsonb_exists(invariant_config, 'tensorboard_args');
