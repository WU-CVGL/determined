import { validate } from 'uuid';

import { expect, test } from 'e2e/fixtures/global-fixtures';
import { TaskLogs } from 'e2e/models/pages/TaskLogs';
import { WorkspaceDetails } from 'e2e/models/pages/WorkspaceDetails';

test.describe('Workspace Tasks', () => {
  test('Shell with JupyterLab', async ({ authedPage, newWorkspace, context }) => {
    const workspaceDetails = new WorkspaceDetails(authedPage);
    const { launchModal, shellLaunchedModal } = workspaceDetails.taskDashboard;
    const firstRow = workspaceDetails.taskDashboard.table.table.rows.nth(0);

    await test.step('Start task', async () => {
      await workspaceDetails.gotoWorkspace(newWorkspace.response.workspace.id);
      await workspaceDetails.jobsTab.pwLocator.click();

      await workspaceDetails.taskDashboard.shellButton.pwLocator.click();
      await launchModal.pwLocator.waitFor();
      // The master runs jupyter by default; the form checks it once the master's preview arrives.
      const jupyter = launchModal.service('jupyter');
      await expect(jupyter.input.pwLocator).toBeChecked();
      const openPages = context.pages().length;
      await launchModal.footer.submit.pwLocator.click();

      // The launch result shows how to connect; nothing opens in a new tab.
      await shellLaunchedModal.pwLocator.waitFor();
      expect(context.pages()).toHaveLength(openPages);
      await shellLaunchedModal.close();

      await firstRow.pwLocator.waitFor({ timeout: 10_000 });
    });

    await test.step('Kill task', async () => {
      await (await firstRow.actions.open()).kill.pwLocator.click();

      await workspaceDetails.taskDashboard.taskKillModal.pwLocator.waitFor();
      await workspaceDetails.taskDashboard.taskKillModal.killButton.pwLocator.click();
      await expect(firstRow.state.pwLocator).toHaveText('Terminated');
    });

    await test.step('Copy task ID', async () => {
      try {
        await context.grantPermissions(['clipboard-read', 'clipboard-write']);
      } catch {
        return;
      }

      await (await firstRow.actions.open()).copy.pwLocator.click();
      const handle = await authedPage.evaluateHandle(() => navigator.clipboard.readText());
      const clipboard = await handle.jsonValue();
      expect(validate(clipboard)).toBeTruthy();
    });

    await test.step('View logs', async () => {
      await (await firstRow.actions.open()).viewLogs.pwLocator.click();

      const taskLogs = new TaskLogs(authedPage);

      await taskLogs.logViewer.pwLocator.waitFor();
      await taskLogs.logViewer.logEntry.nth(0).pwLocator.waitFor();
    });
  });
});
