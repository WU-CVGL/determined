import { BaseComponent } from 'playwright-page-model-base/BaseComponent';
import { BaseOverlay, OverlayArgs } from 'playwright-page-model-base/BaseOverlay';
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
 * Represents a column's tick list filter, TableFilterDropdown in src/components/Table as the Jobs
 * page opens it from the column's funnel.
 */
class ChecklistFilter extends BaseOverlay {
  constructor(args: OverlayArgs) {
    super({
      ...args,
      selector:
        '.ant-dropdown:not(.ant-dropdown-hidden) [aria-label="table-filter-dropdown-container"]',
    });
  }

  readonly search = new BaseComponent({ parent: this, selector: 'input' });
  readonly list = new BaseComponent({ parent: this, selector: '[role="listbox"]' });
  readonly all = new BaseComponent({ parent: this, selector: 'button:has-text("All")' });
  readonly none = new BaseComponent({ parent: this, selector: 'button:has-text("None")' });
  readonly ok = new BaseComponent({ parent: this, selector: 'button:has-text("OK")' });
  readonly option = (text: string): BaseComponent =>
    new BaseComponent({ parent: this, selector: `[role="option"]:has-text("${text}")` });

  /**
   * Closes the filter without applying its ticks.
   */
  async close(): Promise<void> {
    await this.pwLocator.press('Escape');
    await this.pwLocator.waitFor({ state: 'hidden' });
  }
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
  readonly search = new BaseComponent({
    parent: this,
    selector: 'input[placeholder="Search name or ID"]',
  });
  readonly clearFilters = new BaseComponent({
    parent: this,
    selector: 'button:has-text("Clear Filters")',
  });
  /** A column's filter, by its funnel's label, such as "Filter by kind". */
  readonly filter = (label: string): ChecklistFilter =>
    new ChecklistFilter({
      clickThisComponentToOpen: new BaseComponent({
        parent: this,
        selector: `[role="button"][aria-label="${label}"]`,
      }),
      root: this.root,
    });
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
