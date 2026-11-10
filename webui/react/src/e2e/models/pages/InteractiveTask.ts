import {
  BaseComponent,
  ComponentContainer,
  NamedComponent,
} from 'playwright-page-model-base/BaseComponent';
import { BaseReactFragment } from 'playwright-page-model-base/BaseReactFragment';

import { DeterminedPage } from 'e2e/models/common/base/BasePage';
import { DropdownMenu } from 'e2e/models/common/hew/Dropdown';
import { LogViewer } from 'e2e/models/common/hew/LogViewer';
import { Message } from 'e2e/models/common/hew/Message';
import { Pivot } from 'e2e/models/common/hew/Pivot';
import { InteractiveTaskTab } from 'types';

/**
 * Represents the TaskBar from src/components/TaskBar.tsx: the task's name and pool, and its
 * action menu.
 */
class TaskBar extends NamedComponent {
  readonly defaultSelector = '[class^="TaskBar_base"]';
  readonly name = new BaseComponent({
    parent: this,
    selector: '[class^="TaskBar_barContent"] > span:first-child',
  });
  readonly actions = new TaskBarMenu({
    clickThisComponentToOpen: new BaseComponent({
      parent: this,
      selector: '[data-testid="task-action-dropdown-trigger"]',
    }),
    root: this.root,
  });
}

/**
 * Represents the menu of the TaskBar: View Logs, then Kill.
 */
class TaskBarMenu extends DropdownMenu {
  readonly viewLogs = this.menuItem('viewLogs');
  readonly kill = this.menuItem('kill');
}

/**
 * Represents the ShellTerminalPane from src/components/ShellTerminalPane.tsx: the Terminal tab.
 */
class ShellTerminalPane extends BaseReactFragment {
  readonly status = new BaseComponent({
    parent: this,
    selector: '[data-testid="shell-terminal-status"]',
  });
  readonly title = new BaseComponent({
    parent: this,
    selector: '[data-testid="shell-terminal-title"]',
  });
  readonly notice = new BaseComponent({ parent: this, selector: '[role="status"]' });
  readonly reconnect = new BaseComponent({
    parent: this,
    selector: 'button:has-text("Reconnect")',
  });
  readonly copyCommand = new BaseComponent({
    parent: this,
    selector: 'button:has-text("Copy CLI Command")',
  });
  readonly terminal = new BaseComponent({
    parent: this,
    selector: '[data-testid="shell-terminal-view"]',
  });
}

/**
 * Represents a TaskServicePane from src/components/TaskServicePane.tsx: the JupyterLab, VS Code or
 * TensorBoard tab, which loads the service in a frame or shows why it cannot.
 */
class TaskServicePane extends BaseReactFragment {
  readonly frame: BaseComponent;
  readonly message = new Message({ parent: this });
  readonly retry = new BaseComponent({ parent: this, selector: 'button:has-text("Reconnect")' });

  constructor({ label, parent }: { label: string; parent: ComponentContainer }) {
    super({ parent });
    this.frame = new BaseComponent({ parent: this, selector: `iframe[title="${label}"]` });
  }
}

/**
 * Represents the InteractiveTask page from src/pages/InteractiveTask.tsx at
 * /tasks/:taskId/interactive: a task's services, logs and resources as tabs.
 */
export class InteractiveTask extends DeterminedPage {
  readonly title = '';
  readonly url = /tasks\/[0-9a-fA-F-]+\/interactive/;
  readonly taskBar = new TaskBar({ parent: this });
  readonly tabs = new Pivot({ parent: this });
  readonly terminalTab = this.tabs.tab(InteractiveTaskTab.Terminal);
  readonly jupyterTab = this.tabs.tab(InteractiveTaskTab.JupyterLab);
  readonly vscodeTab = this.tabs.tab(InteractiveTaskTab.VSCode);
  readonly tensorBoardTab = this.tabs.tab(InteractiveTaskTab.TensorBoard);
  readonly logsTab = this.tabs.tab(InteractiveTaskTab.Logs);
  readonly resourcesTab = this.tabs.tab(InteractiveTaskTab.Resources);
  readonly terminal = new ShellTerminalPane({ parent: this.tabs.tabContent });
  readonly jupyter = new TaskServicePane({ label: 'JupyterLab', parent: this.tabs.tabContent });
  readonly vscode = new TaskServicePane({ label: 'VS Code', parent: this.tabs.tabContent });
  /** A shell's tensorboard service. */
  readonly tensorBoardService = new TaskServicePane({
    label: 'TensorBoard',
    parent: this.tabs.tabContent,
  });
  readonly logViewer = new LogViewer({ parent: this.tabs.tabContent });
  /** Why the task cannot be shown, in place of the tabs. */
  readonly message = new Message({ parent: this });
}
