import { DEFAULT_COLUMN_WIDTHS, DEFAULT_COLUMNS, withSlotsColumn } from './TaskDashboard.settings';

describe('withSlotsColumn', () => {
  const before = DEFAULT_COLUMNS.filter((col) => col !== 'slots');
  const widths = before.map((col) => DEFAULT_COLUMN_WIDTHS[col] + 1);

  it('puts Slots after Resource Pool in columns stored before it, with its width', () => {
    const update = withSlotsColumn(before, widths);
    expect(update?.columns).toEqual(DEFAULT_COLUMNS);
    const at = DEFAULT_COLUMNS.indexOf('slots');
    expect(update?.columnWidths[at]).toBe(DEFAULT_COLUMN_WIDTHS.slots);
    expect(update?.columnWidths.filter((_, i) => i !== at)).toEqual(widths);
  });

  it('keeps a reordered table in its order', () => {
    const update = withSlotsColumn(['name', 'resourcePool', 'id'], [200, 100, 90]);
    expect(update).toEqual({
      columns: ['name', 'resourcePool', 'slots', 'id'],
      columnWidths: [200, 100, 72, 90],
    });
  });

  it('adds Slots last without Resource Pool, and leaves short widths alone', () => {
    expect(withSlotsColumn(['name', 'id'], [200])).toEqual({
      columns: ['name', 'id', 'slots'],
      columnWidths: [200],
    });
  });

  it('changes nothing once Slots is there', () => {
    expect(withSlotsColumn(DEFAULT_COLUMNS, [])).toBeUndefined();
  });
});
