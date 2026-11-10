import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { UserUsageEntry } from 'types';

import { ResourceMode, usageProjection } from './usage';
import UsageCalendar from './UsageCalendar';

vi.mock('hew/Tooltip');

// A Tuesday: the current UTC week is Monday 2026-11-09 and today.
const NOW = new Date('2026-11-10T15:00:00Z');
const HOUR = 3600;

/** A day with GPU and Aux active hours, 2 GPUs while active and CPU slots. */
const day = (periodStart: string, activeHours: number, auxHours = 0): UserUsageEntry => ({
  auxActiveDays: auxHours > 0 ? 1 : 0,
  auxActiveSeconds: auxHours * HOUR,
  auxContainerSeconds: auxHours * HOUR,
  cpuSlotSeconds: 0.5 * HOUR,
  gpuActiveDays: activeHours > 0 ? 1 : 0,
  gpuActiveSeconds: activeHours * HOUR,
  gpuOrAuxActiveDays: activeHours > 0 || auxHours > 0 ? 1 : 0,
  gpuOrAuxActiveSeconds: Math.max(activeHours, auxHours) * HOUR,
  gpuSeconds: 2 * activeHours * HOUR,
  periodStart,
  unclassifiedSlotSeconds: 0,
});

const setup = (entries: UserUsageEntry[], mode: ResourceMode = ResourceMode.Both) => {
  const projection = usageProjection(mode);
  return render(
    <UIProvider theme={DefaultTheme.Light}>
      <UsageCalendar dayValues={projection.day} weeks={projection.calendar(entries).weeks} />
    </UIProvider>,
  );
};

const cell = (date: string) => screen.getByLabelText(date);

describe('UsageCalendar', () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: NOW, toFake: ['Date'] });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows 53 weeks of 7 days, Monday first, ending with today', () => {
    setup([]);
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
    setup(entries);
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

  it("levels the days by the mode's active hours", () => {
    const { unmount } = setup([day('2026-11-02', 0, 5)]);
    expect(cell('2026-11-02')).toHaveClass('level2');
    unmount();
    setup([day('2026-11-02', 0, 5)], ResourceMode.Gpu);
    expect(cell('2026-11-02')).toHaveClass('level0');
  });

  it("shows a day's date in its tooltip", async () => {
    const user = userEvent.setup();
    setup([day('2026-11-03', 3)]);
    await act(() => user.hover(cell('2026-11-03')));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('2026-11-03');
    expect(screen.queryByTestId('usage-calendar-day')).not.toBeInTheDocument();
  });

  it("shows a day's hours under the grid when clicked", async () => {
    const user = userEvent.setup();
    setup([day('2026-11-03', 3, 1.5)]);
    await act(() => user.click(cell('2026-11-03')));
    const line = screen.getByTestId('usage-calendar-day');
    expect(line).toHaveTextContent('2026-11-03 (UTC)GPU hours 6.0Aux container hours 1.5');
  });

  it("shows a day's hours of the mode on Enter", async () => {
    const user = userEvent.setup();
    setup([day('2026-11-03', 3)], ResourceMode.Gpu);
    act(() => cell('2026-11-03').focus());
    expect(cell('2026-11-03')).toHaveFocus();
    await act(() => user.keyboard('{Enter}'));
    expect(screen.getByTestId('usage-calendar-day')).toHaveTextContent(
      /^2026-11-03 \(UTC\)GPU hours 6.0$/,
    );
  });

  it('shows only the date of a day without records', async () => {
    const user = userEvent.setup();
    setup([]);
    await act(() => user.click(cell('2026-10-31')));
    expect(screen.getByTestId('usage-calendar-day')).toHaveTextContent(/^2026-10-31 \(UTC\)$/);
  });

  it('labels the month columns', () => {
    setup([]);
    expect(screen.getByText('Dec')).toBeInTheDocument();
    expect(screen.getByText('Nov')).toBeInTheDocument();
  });
});
