import { expect, test } from 'e2e/fixtures/global-fixtures';
import { WorkspaceDetails } from 'e2e/models/pages/WorkspaceDetails';
import { safeName } from 'e2e/utils/naming';

/** More templates than the Start from dropdown shows at once (8 rows). */
const TEMPLATE_COUNT = 10;

test.describe('Launch form Start from', () => {
  test('keeps the recent tasks in view when the last of many templates is selected', async ({
    apiAuth,
    authedPage,
    newWorkspace,
  }) => {
    const api = apiAuth.apiContext;
    if (!api) throw new Error('The API context is created at login.');
    const workspaceId = newWorkspace.response.workspace.id;
    const prefix = safeName('start-from');
    const templateNames = Array.from(
      { length: TEMPLATE_COUNT },
      (_, index) => `${prefix}-${String(index).padStart(2, '0')}`,
    );
    const lastTemplate = templateNames[templateNames.length - 1];
    let shellId: string | undefined;

    try {
      await test.step('Create the templates, a recent shell and the last-used template', async () => {
        for (const name of templateNames) {
          const template = await api.post(`/api/v1/templates/${name}`, {
            data: { config: {}, name, workspaceId },
          });
          expect(template.ok()).toBeTruthy();
        }
        const shell = await api.post('/api/v1/shells', {
          data: { config: { resources: { slots: 0 } }, workspaceId },
        });
        expect(shell.ok()).toBeTruthy();
        shellId = (await shell.json()).shell.id;
        const setting = await api.post('/api/v1/users/setting', {
          data: {
            settings: [
              { key: 'template', storagePath: 'shell-launch', value: JSON.stringify(lastTemplate) },
            ],
          },
        });
        expect(setting.ok()).toBeTruthy();
      });

      const workspaceDetails = new WorkspaceDetails(authedPage);
      const launchModal = workspaceDetails.taskList.launchModal;

      await test.step('Open Launch Shell with the last template selected', async () => {
        await workspaceDetails.gotoWorkspace(workspaceId);
        await workspaceDetails.tasksTab.pwLocator.click();
        await workspaceDetails.taskList.shellButton.pwLocator.click();
        await launchModal.pwLocator.waitFor();
        await expect(launchModal.startFrom.selectionItem.pwLocator).toHaveText(lastTemplate);
      });

      await test.step('Open Start from: the selected template and the recent tasks are in view', async () => {
        await launchModal.startFrom.openMenu();
        await expect(launchModal.startFrom.menuItem('Selected').pwLocator).toBeInViewport();
        await expect(launchModal.startFrom.menuItem(lastTemplate).pwLocator).toBeInViewport();
        await expect(
          launchModal.startFrom.menuItem('Recent on cluster').pwLocator,
        ).toBeInViewport();
      });
    } finally {
      if (shellId) await api.post(`/api/v1/shells/${shellId}/kill`);
      for (const name of templateNames) await api.delete(`/api/v1/templates/${name}`);
    }
  });
});
