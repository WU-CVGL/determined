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
    const cells = screen.getAllByRole('img');
    // 52 whole weeks and Monday and Tuesday of this one.
    expect(cells).toHaveLength(52 * 7 + 2);
    expect(cells[0]).toHaveAttribute('data-date', '2025-11-10');
    expect(cells.at(-1)).toHaveAttribute('data-date', '2026-11-10');
    expect(screen.queryByLabelText('2026-11-11')).not.toBeInTheDocument();
  });

  it('levels the covered days by active hours and outlines the days without records', () => {
    const entries = ['2026-11-01', '2026-11-02', '2026-11-03', '2026-11-04', '2026-11-05'].map(
      (date, index) => day(date, [0, 1, 4, 12, 24][index]),
    );
    setup({ usage: usage(entries) });
    expect(cell('2026-11-01')).toHaveClass('level0');
    expect(cell('2026-11-02')).toHaveClass('level1');
    expect(cell('2026-11-03')).toHaveClass('level2');
    expect(cell('2026-11-04')).toHaveClass('level3');
    expect(cell('2026-11-05')).toHaveClass('level3');
    expect(cell('2026-10-31')).toHaveClass('notCovered');
    expect(cell('2026-10-31')).not.toHaveClass('level0');
  });

  it("shows a day's hours in its tooltip", async () => {
    const user = userEvent.setup();
    setup({ showCpuSlots: true, usage: usage([day('2026-11-03', 3, 1.5)]) });
    await act(() => user.hover(cell('2026-11-03')));
    expect(await screen.findByText('GPU hours 6.0')).toBeInTheDocument();
    expect(screen.getByText('CPU slot hours 0.5')).toBeInTheDocument();
    expect(screen.getByText(/^Aux container hours/)).toHaveTextContent('Aux container hours 1.5');
  });

  it('shows Aux as not available where it is unset, and no CPU slots unless shown', async () => {
    const user = userEvent.setup();
    setup({ usage: usage([day('2026-11-03', 3)]) });
    await act(() => user.hover(cell('2026-11-03')));
    expect(await screen.findByText('GPU hours 6.0')).toBeInTheDocument();
    expect(screen.queryByText(/CPU slot hours/)).not.toBeInTheDocument();
    expect(screen.getByText(/^Aux container hours/)).toHaveTextContent(
      'Aux container hours Not available',
    );
  });

  it('labels the month columns', () => {
    setup({ usage: usage([]) });
    expect(screen.getByText('Dec')).toBeInTheDocument();
    expect(screen.getByText('Nov')).toBeInTheDocument();
  });
});
