import { DeterminedPage } from 'e2e/models/common/base/BasePage';
import { UserOverview } from 'e2e/models/components/UserOverview';

/**
 * Represents the Dashboard page from src/pages/Dashboard.tsx: Home, with the signed-in user's
 * overview.
 */
export class Dashboard extends DeterminedPage {
  readonly title = 'Home';
  readonly url = 'dashboard';
  readonly overview = new UserOverview({ parent: this });
}
