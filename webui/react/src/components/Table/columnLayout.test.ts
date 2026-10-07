import { ColumnLayout, UNKNOWN_COLUMN_WIDTH, withColumn } from './columnLayout';

const WIDTHS = { gpus: 170, name: 150, slots: 74, state: 160, user: 85 };

/** The width the table gives each column, which it binds by place. */
const widthOf = ({ columns, columnWidths }: ColumnLayout) => {
  expect(columnWidths).toHaveLength(columns.length);
  return Object.fromEntries(columns.map((col, i) => [col, columnWidths[i]]));
};

describe('withColumn', () => {
  it('inserts the column after its neighbour with its width at the same place', () => {
    const layout = withColumn(
      { columns: ['name', 'slots', 'state', 'user'], columnWidths: [200, 90, 140, 80] },
      'gpus',
      'slots',
      WIDTHS,
    );
    expect(layout.columns).toEqual(['name', 'slots', 'gpus', 'state', 'user']);
    expect(layout.columnWidths).toEqual([200, 90, 170, 140, 80]);
  });

  it('puts the column last without its neighbour', () => {
    const layout = withColumn(
      { columns: ['name', 'user'], columnWidths: [200, 80] },
      'gpus',
      'slots',
      WIDTHS,
    );
    expect(layout.columns).toEqual(['name', 'user', 'gpus']);
    expect(widthOf(layout)).toEqual({ gpus: 170, name: 200, user: 80 });
  });

  it('gives the column its width at its place when the widths are one short', () => {
    const layout = withColumn(
      { columns: ['name', 'slots', 'gpus', 'user'], columnWidths: [200, 90, 80] },
      'gpus',
      'slots',
      WIDTHS,
    );
    expect(widthOf(layout)).toEqual({ gpus: 170, name: 200, slots: 90, user: 80 });
  });

  it('gives a stored name without a default width the width of an unknown column', () => {
    const layout = withColumn(
      { columns: ['name', 'dropped', 'slots'], columnWidths: [200] },
      'gpus',
      'slots',
      WIDTHS,
    );
    expect(widthOf(layout)).toEqual({
      dropped: UNKNOWN_COLUMN_WIDTH,
      gpus: 170,
      name: 200,
      slots: 74,
    });
  });

  it('keeps a layout that has the column, and drops widths past the last column', () => {
    const layout = { columns: ['name', 'slots', 'gpus'], columnWidths: [200, 90, 180] };
    expect(withColumn(layout, 'gpus', 'slots', WIDTHS)).toEqual(layout);
    expect(
      withColumn(
        { ...layout, columnWidths: [...layout.columnWidths, 99] },
        'gpus',
        'slots',
        WIDTHS,
      ),
    ).toEqual(layout);
  });
});
