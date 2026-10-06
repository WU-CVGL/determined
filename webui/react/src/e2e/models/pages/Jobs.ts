import { DeterminedPage } from 'e2e/models/common/base/BasePage';
import { TaskDashboard } from 'e2e/models/components/TaskDashboard';

/**
 * Represents the Jobs page from src/pages/JobsPage.tsx at /jobs: runs of every kind.
 */
export class Jobs extends DeterminedPage {
  readonly title = 'Jobs';
  readonly url = 'jobs';
  readonly taskDashboard = new TaskDashboard({ parent: this });
}

/**
 * Represents the tasks-only view from src/pages/JobsPage.tsx at /tasks: the Jobs page without
 * experiments.
 */
export class Tasks extends DeterminedPage {
  readonly title = 'Tasks';
  readonly url = 'tasks';
  readonly taskDashboard = new TaskDashboard({ parent: this });
}
