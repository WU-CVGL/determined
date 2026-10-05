import { BaseComponent } from 'playwright-page-model-base/BaseComponent';
import { BaseReactFragment } from 'playwright-page-model-base/BaseReactFragment';

import { Modal } from 'e2e/models/common/ant/Modal';
import { DropdownMenu } from 'e2e/models/common/hew/Dropdown';
import { NtscLaunchModal } from 'e2e/models/components/NtscLaunchModal';
import { HeadRow, InteractiveTable, Row } from 'e2e/models/components/Table/InteractiveTable';
import { TaskAction } from 'types';

class TaskHeadRow extends HeadRow {}
class TaskRow extends Row {
  readonly actions = new TaskActionDropdown({
    clickThisComponentToOpen: new BaseComponent({
      parent: this,
      selector: '[data-testid="actions"]',
    }),
    root: this.root,
  });
  readonly state = new BaseComponent({
    parent: this,
    selector: '[data-testid="state"]',
  });
  readonly taskID = new BaseComponent({
    parent: this,
    selector: '[data-testid="taskID"]',
  });
}

/**
 * Represents the TaskActionDropdown from src/components/TaskActionDropdown.tsx
 */
class TaskActionDropdown extends DropdownMenu {
  readonly kill = this.menuItem(TaskAction.Kill);
  readonly copy = this.menuItem(TaskAction.CopyTaskID);
  readonly viewLogs = this.menuItem(TaskAction.ViewLogs);
  readonly connect = this.menuItem(TaskAction.Connect);
}

class TaskKillModal extends Modal {
  readonly killButton = new BaseComponent({
    parent: this,
    selector: '.ant-btn-dangerous',
  });
}

/**
 * Represents the TaskDashboard in src/components/TaskDashboard/TaskDashboard.tsx: the Jobs page,
 * the tasks-only view at /tasks, and the Jobs tabs of workspaces and projects.
 */
export class TaskDashboard extends BaseReactFragment {
  readonly jupyterLabButton = new BaseComponent({
    parent: this,
    selector: '[data-testid="jupyter-lab-button"]',
  });
  readonly launchModal = new NtscLaunchModal({
    root: this.root,
  });
  readonly shellButton = new BaseComponent({
    parent: this,
    selector: '[data-testid="shell-button"]',
  });
  readonly kindChip = (kind: string): BaseComponent =>
    new BaseComponent({ parent: this, selector: `[data-testid="kind-${kind}"]` });
  readonly table = new InteractiveTable({
    parent: this,
    tableArgs: {
      attachment: '[data-testid="table"]',
      headRowType: TaskHeadRow,
      rowType: TaskRow,
    },
  });
  readonly taskKillModal = new TaskKillModal({
    root: this.root,
  });
}
