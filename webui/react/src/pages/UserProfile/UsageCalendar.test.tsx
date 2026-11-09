import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { UserUsage, UserUsageEntry } from 'types';

import UsageCalendar from './UsageCalendar';

vi.mock('hew/Tooltip');

// A Tuesday: the current UTC week is Monday 2026-11-09 and today.
const NOW = new Date('2026-11-10T15:00:00Z');
const HOUR = 3600;

const day = (periodStart: string, activeHours: number, aux?: number): UserUsageEntry => ({
  activeDays: activeHours > 0 ? 1 : 0,
  activeSeconds: activeHours * HOUR,
  auxContainerSeconds: aux === undefined ? undefined : aux * HOUR,
  cpuSlotSeconds: 0.5 * HOUR,
  gpuSeconds: 2 * activeHours * HOUR,
  periodStart,
});

/** Covered from 2026-11-01: the days before have no entries. */
const usage = (entries: UserUsageEntry[]): UserUsage => ({
  coverageStart: '2026-11-01',
  entries,
});

const setup = (props: { showCpuSlots?: boolean; usage: UserUsage }) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <UsageCalendar showCpuSlots={props.showCpuSlots ?? false} usage={props.usage} />
    </UIProvider>,
  );

const cell = (date: string) => screen.getByLabelText(date);

describe('UsageCalendar', () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: NOW, toFake: ['Date'] });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows 53 weeks of 7 days, Monday first, ending with today', () => {
    setup({ usage: usage([]) });
    const cells = screen.getAllByRole('button');
    // 52 whole weeks and Monday and Tuesday of this one.
    expect(cells).toHaveLength(52 * 7 + 2);
    expect(cells[0]).toHaveAttribute('data-date', '2025-11-10');
    expect(cells.at(-1)).toHaveAttribute('data-date', '2026-11-10');
    expect(screen.queryByLabelText('2026-11-11')).not.toBeInTheDocument();
  });

  it('levels the covered days by active hours and outlines the days without records', () => {
    const hours = [0, 1, 4, 5, 12, 13, 24];
    const entries = hours.map((active, index) => day(`2026-11-0${index + 1}`, active));
    setup({ usage: usage(entries) });
    expect(cell('2026-11-01')).toHaveClass('level0');
    expect(cell('2026-11-02')).toHaveClass('level1');
    expect(cell('2026-11-03')).toHaveClass('level1');
    expect(cell('2026-11-04')).toHaveClass('level2');
    expect(cell('2026-11-05')).toHaveClass('level2');
    expect(cell('2026-11-06')).toHaveClass('level3');
    expect(cell('2026-11-07')).toHaveClass('level3');
    expect(cell('2026-10-31')).toHaveClass('notCovered');
    expect(cell('2026-10-31')).not.toHaveClass('level0');
  });

  it("shows a day's date in its tooltip", async () => {
    const user = userEvent.setup();
    setup({ usage: usage([day('2026-11-03', 3)]) });
    await act(() => user.hover(cell('2026-11-03')));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('2026-11-03');
    expect(screen.queryByTestId('usage-calendar-day')).not.toBeInTheDocument();
  });

  it("shows a day's hours under the grid when clicked", async () => {
    const user = userEvent.setup();
    setup({ showCpuSlots: true, usage: usage([day('2026-11-03', 3, 1.5)]) });
    await act(() => user.click(cell('2026-11-03')));
    const line = screen.getByTestId('usage-calendar-day');
    expect(line).toHaveTextContent(
      '2026-11-03GPU hours 6.0CPU slot hours 0.5Aux container hours 1.5',
    );
  });

  it("shows a day's hours on Enter, Aux as not available where it is unset, and no CPU slots unless shown", async () => {
    const user = userEvent.setup();
    setup({ usage: usage([day('2026-11-03', 3)]) });
    act(() => cell('2026-11-03').focus());
    expect(cell('2026-11-03')).toHaveFocus();
    await act(() => user.keyboard('{Enter}'));
    expect(screen.getByTestId('usage-calendar-day')).toHaveTextContent(
      '2026-11-03GPU hours 6.0Aux container hours Not available',
    );
  });

  it('shows only the date of a day without records', async () => {
    const user = userEvent.setup();
    setup({ usage: usage([]) });
    await act(() => user.click(cell('2026-10-31')));
    expect(screen.getByTestId('usage-calendar-day')).toHaveTextContent(/^2026-10-31$/);
  });

  it('labels the month columns', () => {
    setup({ usage: usage([]) });
    expect(screen.getByText('Dec')).toBeInTheDocument();
    expect(screen.getByText('Nov')).toBeInTheDocument();
  });
});
