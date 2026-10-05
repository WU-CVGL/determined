import { BaseComponent } from 'playwright-page-model-base/BaseComponent';

import { Modal } from 'e2e/models/common/hew/Modal';
import { Select } from 'e2e/models/common/hew/Select';

/**
 * Represents the launch form for JupyterLab and shells in src/components/NtscLaunchModal.tsx,
 * which Launch JupyterLab, Launch Shell and Launch Again open with a task type selected.
 */
export class NtscLaunchModal extends Modal {
  readonly startFrom = new Select({
    parent: this,
    selector: '[data-test-component="start-from-select"] .ant-select',
  });
  readonly typeSelect = new LaunchTypeSelect({
    parent: this,
    selector: '[data-test-component="launch-type-select"]',
  });
}

/**
 * The task type selector at the top of the launch form.
 */
class LaunchTypeSelect extends BaseComponent {
  readonly jupyterLab = new BaseComponent({ parent: this, selector: 'label:first-of-type' });
  readonly shell = new BaseComponent({ parent: this, selector: 'label:last-of-type' });
}
