/** The columns of a table and their widths, which the table binds by place. */
export interface ColumnLayout<C extends string = string> {
  columns: C[];
  columnWidths: number[];
}

/** The width of a stored column that has no default width, such as one a later version dropped. */
export const UNKNOWN_COLUMN_WIDTH = 100;

const insertAt = <T>(items: T[], at: number, item: T): T[] => [
  ...items.slice(0, at),
  item,
  ...items.slice(at),
];

/**
 * The layout with the named column and one width for each column, for a stored layout from before
 * the column:
 * - Columns without it get it after `after` (else last) at its default width, and each keeps its
 *   own width.
 * - Widths one short of columns that have it, as a resize stores them for columns from before it,
 *   get its default width at its place.
 * - A missing width is its column's default (UNKNOWN_COLUMN_WIDTH for a name without one); widths
 *   past the last column are dropped.
 */
export const withColumn = <C extends string>(
  { columns, columnWidths }: ColumnLayout<C>,
  name: C,
  after: C,
  defaultWidths: Partial<Record<C, number>>,
): ColumnLayout<C> => {
  const widthOf = (col: C) => defaultWidths[col] ?? UNKNOWN_COLUMN_WIDTH;
  let cols = columns;
  let widths = columnWidths;
  if (!cols.includes(name)) {
    const at = cols.includes(after) ? cols.indexOf(after) + 1 : cols.length;
    const own = cols.map((col, i) => widths[i] ?? widthOf(col));
    cols = insertAt(cols, at, name);
    widths = insertAt(own, at, widthOf(name));
  } else if (widths.length === cols.length - 1) {
    widths = insertAt(widths, cols.indexOf(name), widthOf(name));
  }
  return { columns: cols, columnWidths: cols.map((col, i) => widths[i] ?? widthOf(col)) };
};
