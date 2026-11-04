import { BaseComponent } from 'playwright-page-model-base/BaseComponent';

import { Modal } from 'e2e/models/common/hew/Modal';
import { Select } from 'e2e/models/common/hew/Select';

/** The form's label of each service's checkbox, by the service's config key. */
const SERVICE_LABELS = { jupyter: 'JupyterLab', vscode: 'VS Code' } as const;

/**
 * Represents the shell launch form in src/components/NtscLaunchModal.tsx, which Launch Interactive
 * Task and Launch Again open.
 */
export class NtscLaunchModal extends Modal {
  readonly startFrom = new Select({
    parent: this,
    selector: '[data-test-component="start-from-select"] .ant-select',
  });
  // The simple form's fields, by their labels.
  readonly name = new BaseComponent({
    parent: this,
    selector: '.ant-form-item:has(label[title="Name"]) input',
  });
  readonly pool = new Select({
    parent: this,
    selector: '.ant-form-item:has(label[title="Resource Pool"]) .ant-select',
  });
  readonly slots = new BaseComponent({
    parent: this,
    selector: '.ant-form-item:has(label[title="Slots"]) input',
  });
  /** The checkbox of a service the shell runs besides sshd, such as jupyter, by its config key. */
  readonly service = (name: keyof typeof SERVICE_LABELS): ServiceCheckbox =>
    new ServiceCheckbox({
      parent: this,
      selector: `.ant-form-item:has(label[title="Services"]) label.ant-checkbox-wrapper:has(span:text-is("${SERVICE_LABELS[name]}"))`,
    });
}

/**
 * A service's checkbox: its label, which checks or unchecks it, and its input.
 */
class ServiceCheckbox extends BaseComponent {
  readonly input = new BaseComponent({ parent: this, selector: 'input[type="checkbox"]' });
}
