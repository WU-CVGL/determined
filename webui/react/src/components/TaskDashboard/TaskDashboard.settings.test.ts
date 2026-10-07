import settingsConfig, {
  ColumnLayout,
  DEFAULT_COLUMN_WIDTHS,
  DEFAULT_COLUMNS,
  defaultWidths,
  normalizedLayout,
  TaskDashboardColumnName,
} from './TaskDashboard.settings';

/* The default columns before the Slots column, and widths that a user gave them. */
const OLD_COLUMNS = DEFAULT_COLUMNS.filter((col) => col !== 'slots');
const OLD_WIDTHS = [301, 302, 303, 304, 305, 306, 307, 308, 309];
const OWN_WIDTHS = {
  endTime: 309,
  id: 302,
  kind: 301,
  location: 306,
  name: 303,
  resourcePool: 307,
  startTime: 308,
  state: 304,
  user: 305,
};

/** The width the table gives each column, which it binds by place. */
const widthOf = ({ columns, columnWidths }: ColumnLayout) => {
  expect(columnWidths).toHaveLength(columns.length);
  return Object.fromEntries(columns.map((col, i) => [col, columnWidths[i]]));
};

/** The layout the table shows: normalized, which a second pass leaves alone, or as stored. */
const normalized = (layout: ColumnLayout): ColumnLayout => {
  const update = normalizedLayout(layout);
  if (update) expect(normalizedLayout(update)).toBeUndefined();
  return update ?? layout;
};

const defaultsOf = (columns: TaskDashboardColumnName[]) =>
  columns.map((col) => DEFAULT_COLUMN_WIDTHS[col]);

describe('normalizedLayout', () => {
  it('changes nothing without stored columns or widths', () => {
    expect(
      normalizedLayout({ columns: DEFAULT_COLUMNS, columnWidths: defaultWidths() }),
    ).toBeUndefined();
  });

  it('puts Slots after Resource Pool in columns stored before it, each column with its width', () => {
    const layout = normalized({ columns: OLD_COLUMNS, columnWidths: OLD_WIDTHS });
    expect(layout.columns).toEqual(DEFAULT_COLUMNS);
    expect(widthOf(layout)).toEqual({ ...OWN_WIDTHS, slots: DEFAULT_COLUMN_WIDTHS.slots });
  });

  it('gives Slots its width at its place when only the widths before it were stored', () => {
    // The settings give the default columns of now for columns never stored, Slots included.
    const layout = normalized({ columns: DEFAULT_COLUMNS, columnWidths: OLD_WIDTHS });
    expect(layout.columns).toEqual(DEFAULT_COLUMNS);
    expect(widthOf(layout)).toEqual({ ...OWN_WIDTHS, slots: DEFAULT_COLUMN_WIDTHS.slots });
  });

  it('gives columns stored without widths their own default widths', () => {
    // The settings give the default widths, which are those of the default columns.
    expect(normalized({ columns: OLD_COLUMNS, columnWidths: defaultWidths() })).toEqual({
      columns: DEFAULT_COLUMNS,
      columnWidths: defaultWidths(),
    });

    const reordered: TaskDashboardColumnName[] = ['name', 'kind', 'resourcePool', 'endTime'];
    const layout = normalized({ columns: reordered, columnWidths: defaultWidths() });
    expect(layout.columns).toEqual(['name', 'kind', 'resourcePool', 'slots', 'endTime']);
    expect(layout.columnWidths).toEqual(defaultsOf(layout.columns));
  });

  it('keeps a reordered table in its order, each column with its width', () => {
    const layout = normalized({
      columns: ['name', 'resourcePool', 'id', 'kind'],
      columnWidths: [250, 140, 90, 70],
    });
    expect(layout.columns).toEqual(['name', 'resourcePool', 'slots', 'id', 'kind']);
    expect(widthOf(layout)).toEqual({ id: 90, kind: 70, name: 250, resourcePool: 140, slots: 72 });
  });

  it('adds Slots last without Resource Pool', () => {
    expect(normalized({ columns: ['name', 'id'], columnWidths: [250, 90] })).toEqual({
      columns: ['name', 'id', 'slots'],
      columnWidths: [250, 90, DEFAULT_COLUMN_WIDTHS.slots],
    });
  });

  it('changes nothing once Slots is there and each column has a width', () => {
    const widths = defaultsOf(DEFAULT_COLUMNS).map((width) => width + 10);
    expect(normalizedLayout({ columns: DEFAULT_COLUMNS, columnWidths: widths })).toBeUndefined();
    expect(
      normalizedLayout({ columns: ['slots', 'name', 'kind'], columnWidths: [80, 230, 70] }),
    ).toBeUndefined();
  });

  it('keeps a width that the table changed in place in the default widths', () => {
    // The table resizes in the widths it was given, the settings' default when none were stored.
    const widths = settingsConfig({ type: 'global' }, true).settings.columnWidths.defaultValue;
    widths[2] = 400;
    expect(normalizedLayout({ columns: DEFAULT_COLUMNS, columnWidths: widths })).toBeUndefined();
  });

  it('gives each column without a width its default width', () => {
    expect(
      normalized({ columns: ['name', 'resourcePool', 'id', 'kind'], columnWidths: [250] }),
    ).toEqual({
      columns: ['name', 'resourcePool', 'slots', 'id', 'kind'],
      columnWidths: [250, 128, 72, 100, 64],
    });
    expect(normalized({ columns: ['slots', 'name', 'id', 'kind'], columnWidths: [80] })).toEqual({
      columns: ['slots', 'name', 'id', 'kind'],
      columnWidths: [80, 240, 100, 64],
    });
  });

  it('drops widths past the last column', () => {
    expect(
      normalized({ columns: ['name', 'resourcePool', 'id'], columnWidths: [250, 140, 90, 1, 2] }),
    ).toEqual({
      columns: ['name', 'resourcePool', 'slots', 'id'],
      columnWidths: [250, 140, 72, 90],
    });
    expect(normalized({ columns: ['slots', 'name'], columnWidths: [80, 250, 1, 2] })).toEqual({
      columns: ['slots', 'name'],
      columnWidths: [80, 250],
    });
  });

  it('reads no columns as the default ones', () => {
    expect(normalized({ columns: [], columnWidths: OLD_WIDTHS })).toEqual({
      columns: DEFAULT_COLUMNS,
      columnWidths: [301, 302, 303, 304, 305, 306, 307, 72, 308, 309],
    });
  });
});
