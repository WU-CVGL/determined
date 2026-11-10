import { BaseComponent } from 'playwright-page-model-base/BaseComponent';

import { DeterminedPage } from 'e2e/models/common/base/BasePage';
import { Message } from 'e2e/models/common/hew/Message';
import { UserBadge } from 'e2e/models/components/UserBadge';
import { UserOverview } from 'e2e/models/components/UserOverview';

/**
 * Represents the UserProfile page from src/pages/UserProfile/UserProfile.tsx at /users/:userId:
 * another user's page. The signed-in user's own id redirects to Home.
 */
export class UserProfile extends DeterminedPage {
  readonly title = '';
  readonly url = /users\/(\d+)/;

  /**
   * Visits a user's page.
   * @param {number} userId - the user's ID
   */
  async gotoUser(userId: number, args = {}): Promise<this> {
    return await this.goto({ ...args, url: `users/${userId}` });
  }

  async getIdFromUrl(): Promise<number> {
    await this._page.waitForURL(this.url);
    const matches = new URL(this._page.url()).pathname.match(this.url);
    if (matches === null) throw new Error('No ID found in the URL');
    return Number(matches[1]);
  }

  readonly #base = new BaseComponent({ parent: this, selector: 'article#userProfile' });
  readonly #header = new BaseComponent({
    parent: this.#base,
    selector: '[class*="UserProfile_header"]',
  });
  readonly userBadge = new UserBadge({ parent: this.#header });
  readonly deactivated = new BaseComponent({
    parent: this.#header,
    selector: '[class*="Badge_base"]:text-is("Deactivated")',
  });
  readonly jobsLink = new BaseComponent({
    parent: this.#header,
    selector: 'a[href*="/jobs?user="]',
  });
  /** "User not found" or "Unable to fetch user" in place of the page. */
  readonly message = new Message({ parent: this.#base });
  readonly overview = new UserOverview({ parent: this.#base });
}
