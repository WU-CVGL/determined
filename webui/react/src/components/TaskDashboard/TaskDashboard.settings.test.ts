import settingsConfig, {
  ColumnLayout,
  DEFAULT_COLUMN_WIDTHS,
  DEFAULT_COLUMNS,
  defaultWidths,
  normalizedLayout,
  readFilters,
  Settings,
  TaskDashboardColumnName,
  urlView,
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
    expect(widthOf(layout)).toEqual({
      id: 90,
      kind: 70,
      name: 250,
      resourcePool: 140,
      slots: DEFAULT_COLUMN_WIDTHS.slots,
    });
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
      columnWidths: [
        250,
        DEFAULT_COLUMN_WIDTHS.resourcePool,
        DEFAULT_COLUMN_WIDTHS.slots,
        100,
        DEFAULT_COLUMN_WIDTHS.kind,
      ],
    });
    expect(normalized({ columns: ['slots', 'name', 'id', 'kind'], columnWidths: [80] })).toEqual({
      columns: ['slots', 'name', 'id', 'kind'],
      columnWidths: [80, 240, 100, DEFAULT_COLUMN_WIDTHS.kind],
    });
  });

  it('drops widths past the last column', () => {
    expect(
      normalized({ columns: ['name', 'resourcePool', 'id'], columnWidths: [250, 140, 90, 1, 2] }),
    ).toEqual({
      columns: ['name', 'resourcePool', 'slots', 'id'],
      columnWidths: [250, 140, DEFAULT_COLUMN_WIDTHS.slots, 90],
    });
    expect(normalized({ columns: ['slots', 'name'], columnWidths: [80, 250, 1, 2] })).toEqual({
      columns: ['slots', 'name'],
      columnWidths: [80, 250],
    });
  });

  it('reads no columns as the default ones', () => {
    expect(normalized({ columns: [], columnWidths: OLD_WIDTHS })).toEqual({
      columns: DEFAULT_COLUMNS,
      columnWidths: [301, 302, 303, 304, 305, 306, 307, DEFAULT_COLUMN_WIDTHS.slots, 308, 309],
    });
  });
});

describe('readFilters', () => {
  const read = (saved: Record<string, unknown>, userId?: number) =>
    readFilters({ ...saved } as unknown as Settings, userId);

  it('reads the filters of now as saved, with nothing to clean', () => {
    const saved = {
      search: 'bert',
      slots: ['0', 'multi:8'],
      state: ['active'],
      type: ['shell'],
      user: [3],
      workspace: [7, 9],
    };
    expect(read(saved, 3)).toEqual({ cleanup: undefined, filters: saved, waitsForUser: false });
  });

  it('cleans what 0.41.0 saved: GPU, CPU-only, one workspace and Mine', () => {
    expect(read({ owner: 'mine', slots: 'gpu', workspace: 7 }, 3)).toEqual({
      cleanup: { owner: undefined, slots: ['multi:0'], user: [3], workspace: [7] },
      filters: {
        search: undefined,
        slots: ['multi:0'],
        state: undefined,
        type: undefined,
        user: [3],
        workspace: [7],
      },
      waitsForUser: false,
    });
    expect(read({ owner: 'all', slots: 'cpu-only' }, 3).cleanup).toEqual({
      owner: undefined,
      slots: ['0'],
    });
  });

  it('waits for the signed-in user for Mine, and cleans the rest meanwhile', () => {
    expect(read({ owner: 'mine', slots: 'gpu' })).toMatchObject({
      cleanup: { slots: ['multi:0'] },
      filters: { user: undefined },
      waitsForUser: true,
    });
  });

  it('drops values it does not know', () => {
    expect(read({ slots: ['x', '2'], state: ['RUNNING'], type: ['bogus'] }, 3)).toMatchObject({
      cleanup: { slots: ['2'] },
      filters: { slots: ['2'], state: undefined, type: undefined },
    });
  });
});

describe('urlView', () => {
  it('leaves the saved view to a URL without any of its keys', () => {
    expect(urlView('')).toBeUndefined();
    expect(urlView('?tab=runs')).toBeUndefined();
  });

  it('sets every filter, the sort and the page from a URL with any of them', () => {
    expect(urlView('?state=paused')).toEqual({
      owner: undefined,
      search: undefined,
      slots: undefined,
      sortDesc: true,
      sortKey: 'startTime',
      state: ['paused'],
      tableLimit: 20,
      tableOffset: 0,
      type: undefined,
      user: undefined,
      workspace: undefined,
    });
    expect(
      urlView(
        '?type=shell&type=experiment&search=bert&user=3&user=4&slots=0&slots=multi%3A8' +
          '&workspace=7&sortKey=name&sortDesc=false&tableOffset=40&tableLimit=50',
      ),
    ).toEqual({
      owner: undefined,
      search: 'bert',
      slots: ['0', 'multi:8'],
      sortDesc: false,
      sortKey: 'name',
      state: undefined,
      tableLimit: 50,
      tableOffset: 40,
      type: ['shell', 'experiment'],
      user: [3, 4],
      workspace: [7],
    });
  });

  it('keeps old /tasks links: kinds, owners, workspaces and the Type sort, not old states', () => {
    expect(
      urlView('?type=jupyter-lab&user=5&workspace=2&sortKey=type&sortDesc=false&state=RUNNING'),
    ).toMatchObject({
      sortDesc: false,
      sortKey: 'type',
      state: undefined,
      type: ['jupyter-lab'],
      user: [5],
      workspace: [2],
    });
    expect(urlView('?sortKey=id')).toMatchObject({ sortKey: 'startTime' });
    expect(urlView('?sortKey=workspace&sortDesc=false')).toMatchObject({
      sortDesc: true,
      sortKey: 'startTime',
    });
  });

  it("reads 0.41.0's GPU, CPU-only and Mine", () => {
    expect(urlView('?slots=gpu&owner=mine', 3)).toMatchObject({ slots: ['multi:0'], user: [3] });
    expect(urlView('?slots=cpu-only')).toMatchObject({ slots: ['0'] });
  });
});
