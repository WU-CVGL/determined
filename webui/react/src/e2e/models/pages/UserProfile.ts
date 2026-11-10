import { BaseComponent } from 'playwright-page-model-base/BaseComponent';

import { HeadRow, Row, Table } from 'e2e/models/common/ant/Table';
import { DeterminedPage } from 'e2e/models/common/base/BasePage';
import { Message } from 'e2e/models/common/hew/Message';
import { UserBadge } from 'e2e/models/components/UserBadge';

/**
 * Represents a stat of the Current resources section, OverviewStats in
 * src/components/OverviewStats.tsx: its title and its value.
 */
class ResourceStat extends BaseComponent {
  readonly value = new BaseComponent({
    parent: this,
    selector: '[class*="Typography_strong"]',
  });
}

/**
 * Represents the Current resources section of the UserProfile page.
 */
class CurrentResources extends BaseComponent {
  readonly gpus = this.stat('GPUs');
  readonly auxContainers = this.stat('Aux containers');

  /**
   * Returns the stat with the given title.
   * @param {string} title - the stat's title
   */
  stat(title: string): ResourceStat {
    return new ResourceStat({
      parent: this,
      selector: `[class*="Card_cardBase"]:has(:text-is("${title}"))`,
    });
  }
}

/**
 * Represents UsageCalendar from src/pages/UserProfile/UsageCalendar.tsx: a cell for each UTC day.
 */
class UsageCalendar extends BaseComponent {
  readonly days = new BaseComponent({ parent: this, selector: '[data-date]' });

  /**
   * Returns the cell of a UTC day.
   * @param {string} date - the day as YYYY-MM-DD
   */
  day(date: string): BaseComponent {
    return new BaseComponent({ parent: this, selector: `[data-date="${date}"]` });
  }
}

/**
 * Represents the Usage section of the UserProfile page: the Day | Month switch, the chart and the
 * table.
 */
class UsageSection extends BaseComponent {
  readonly day = new BaseComponent({
    parent: this,
    selector: 'label:has-text("Day")',
  });
  readonly month = new BaseComponent({
    parent: this,
    selector: 'label:has-text("Month")',
  });
}

class UsageHeadRow extends HeadRow {}
/** A row of the usage table, keyed by its UTC day or month (YYYY-MM-DD or YYYY-MM). */
class UsageRow extends Row {}

/**
 * Represents the UserProfile page from src/pages/UserProfile/UserProfile.tsx at /users/:userId.
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
  /** "User not found" in place of the page. */
  readonly notFound = new Message({ parent: this.#base });
  readonly currentResources = new CurrentResources({
    parent: this.#base,
    selector: 'section#current-resources',
  });
  readonly #activeDays = new BaseComponent({
    parent: this.#base,
    selector: 'section#active-days',
  });
  readonly calendar = new UsageCalendar({
    parent: this.#activeDays,
    selector: '[data-testid="usage-calendar"]',
  });
  readonly usage = new UsageSection({ parent: this.#base, selector: 'section#usage' });
  readonly usageChart = new BaseComponent({
    parent: this.usage,
    selector: '[class^="ClusterHistoricalUsageChart_base"]',
  });
  readonly usageTable = new Table({
    bodySelector: 'tbody.ant-table-tbody',
    headRowType: UsageHeadRow,
    headSelector: 'thead.ant-table-thead',
    parent: this.usage,
    rowType: UsageRow,
  });
}
