import { BaseComponent, NamedComponent } from 'playwright-page-model-base/BaseComponent';

import { HeadRow, Row, Table } from 'e2e/models/common/ant/Table';

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
 * Represents the Current resources section of the overview.
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
 * Represents the resource mode control of the overview: GPU, Aux or GPU+Aux, shown unless the
 * cluster does not meter Aux containers and the usage holds no Aux time.
 */
class ModeControl extends BaseComponent {
  readonly gpu = this.option('gpu');
  readonly aux = this.option('aux');
  readonly both = this.option('both');

  /**
   * Returns the button of a mode.
   * @param {string} mode - gpu, aux or both
   */
  option(mode: string): BaseComponent {
    return new BaseComponent({ parent: this, selector: `label:has(input[value="${mode}"])` });
  }
}

/**
 * Represents UsageCalendar from src/pages/UserProfile/UsageCalendar.tsx: a cell for each UTC day.
 */
class UsageCalendar extends BaseComponent {
  readonly days = new BaseComponent({ parent: this, selector: '[data-date]' });
  /** The date and the values of the selected day. */
  readonly dayLine = new BaseComponent({
    parent: this,
    selector: '[data-testid="usage-calendar-day"]',
  });

  /**
   * Returns the cell of a UTC day.
   * @param {string} date - the day as YYYY-MM-DD
   */
  day(date: string): BaseComponent {
    return new BaseComponent({ parent: this, selector: `[data-date="${date}"]` });
  }
}

/**
 * Represents the Usage section of the overview: the Day | Month switch, the chart and the table.
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
 * Represents UserOverview from src/pages/UserProfile/UserOverview.tsx, on Home and on the pages
 * of other users.
 */
export class UserOverview extends NamedComponent {
  override defaultSelector = '[data-testid="user-overview"]';
  readonly mode = new ModeControl({
    parent: this,
    selector: '[class*="UserOverview_mode"]',
  });
  readonly currentResources = new CurrentResources({
    parent: this,
    selector: 'section#current-resources',
  });
  readonly activeDays = new BaseComponent({
    parent: this,
    selector: 'section#active-days',
  });
  /** "Active days · N". */
  readonly activeDaysTitle = new BaseComponent({
    parent: this.activeDays,
    selector: 'h5',
  });
  readonly calendar = new UsageCalendar({
    parent: this.activeDays,
    selector: '[data-testid="usage-calendar"]',
  });
  readonly usage = new UsageSection({ parent: this, selector: 'section#usage' });
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
