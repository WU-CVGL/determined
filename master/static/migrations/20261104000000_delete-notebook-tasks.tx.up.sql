-- Delete the NOTEBOOK tasks with their allocations, logs, stats, sessions and jobs. The NOTEBOOK
-- values of the task_type and job_type enums stay.
CREATE TEMP TABLE notebook_tasks ON COMMIT DROP AS
SELECT task_id FROM tasks WHERE task_type = 'NOTEBOOK';

CREATE TEMP TABLE notebook_allocations ON COMMIT DROP AS
SELECT allocation_id FROM allocations WHERE task_id IN (SELECT task_id FROM notebook_tasks);

-- Rows of the notebooks' allocations that do not cascade.
DELETE FROM task_stats WHERE allocation_id IN (SELECT allocation_id FROM notebook_allocations);
DELETE FROM allocation_sessions
WHERE allocation_id IN (SELECT allocation_id FROM notebook_allocations);
DELETE FROM allocation_workspace_info
WHERE allocation_id IN (SELECT allocation_id FROM notebook_allocations);
DELETE FROM resourcemanagers_dispatcher_dispatches
WHERE allocation_id IN (SELECT allocation_id FROM notebook_allocations);
DELETE FROM checkpoints_v2
WHERE allocation_id IN (SELECT allocation_id FROM notebook_allocations)
    OR task_id IN (SELECT task_id FROM notebook_tasks);

-- Rows of the notebook tasks that do not cascade. Shells' JupyterLab tokens share notebook_sessions.
DELETE FROM task_logs WHERE task_id IN (SELECT task_id FROM notebook_tasks);
DELETE FROM log_policy_dont_retry WHERE task_id IN (SELECT task_id FROM notebook_tasks);
DELETE FROM log_policy_retry_on_different_node WHERE task_id IN (SELECT task_id FROM notebook_tasks);
DELETE FROM notebook_sessions WHERE task_id IN (SELECT task_id FROM notebook_tasks);

-- Cascades allocation_resources, allocation_accelerators and command_state.
DELETE FROM allocations WHERE allocation_id IN (SELECT allocation_id FROM notebook_allocations);
-- Cascades the remaining rows keyed by task_id.
DELETE FROM tasks WHERE task_id IN (SELECT task_id FROM notebook_tasks);

DELETE FROM jobs j
WHERE j.job_type = 'NOTEBOOK'
    AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.job_id)
    AND NOT EXISTS (SELECT 1 FROM experiments e WHERE e.job_id = j.job_id);
