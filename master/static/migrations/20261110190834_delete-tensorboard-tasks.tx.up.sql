-- Delete the TENSORBOARD tasks with their allocations, logs, stats, sessions and jobs. The
-- TENSORBOARD values of the task_type and job_type enums stay. Event files in storage stay.
CREATE TEMP TABLE tensorboard_tasks ON COMMIT DROP AS
SELECT task_id FROM tasks WHERE task_type = 'TENSORBOARD';

CREATE TEMP TABLE tensorboard_allocations ON COMMIT DROP AS
SELECT allocation_id FROM allocations WHERE task_id IN (SELECT task_id FROM tensorboard_tasks);

-- Rows of the TensorBoards' allocations that do not cascade.
DELETE FROM task_stats WHERE allocation_id IN (SELECT allocation_id FROM tensorboard_allocations);
DELETE FROM allocation_sessions
WHERE allocation_id IN (SELECT allocation_id FROM tensorboard_allocations);
DELETE FROM allocation_workspace_info
WHERE allocation_id IN (SELECT allocation_id FROM tensorboard_allocations);
DELETE FROM resourcemanagers_dispatcher_dispatches
WHERE allocation_id IN (SELECT allocation_id FROM tensorboard_allocations);
DELETE FROM checkpoints_v2
WHERE allocation_id IN (SELECT allocation_id FROM tensorboard_allocations)
    OR task_id IN (SELECT task_id FROM tensorboard_tasks);

-- Rows of the TensorBoard tasks that do not cascade.
DELETE FROM task_logs WHERE task_id IN (SELECT task_id FROM tensorboard_tasks);
DELETE FROM log_policy_dont_retry WHERE task_id IN (SELECT task_id FROM tensorboard_tasks);
DELETE FROM log_policy_retry_on_different_node
WHERE task_id IN (SELECT task_id FROM tensorboard_tasks);

-- Cascades allocation_resources (with the agent container rows), allocation_accelerators and
-- command_state.
DELETE FROM allocations WHERE allocation_id IN (SELECT allocation_id FROM tensorboard_allocations);
-- Cascades the remaining rows keyed by task_id.
DELETE FROM tasks WHERE task_id IN (SELECT task_id FROM tensorboard_tasks);

DELETE FROM jobs j
WHERE j.job_type = 'TENSORBOARD'
    AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.job_id)
    AND NOT EXISTS (SELECT 1 FROM experiments e WHERE e.job_id = j.job_id);
