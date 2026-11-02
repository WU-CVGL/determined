const TIME = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:?\d{2})?$/i;

/**
 * A time as its milliseconds since the epoch to the second and its nanoseconds within the second:
 * the APIs write times to the microsecond or nanosecond, which Date.parse cuts to the millisecond.
 * Undefined for no time.
 */
export const timeKey = (time?: string): [number, number] | undefined => {
  if (!time) return undefined;
  const parts = TIME.exec(time);
  if (!parts) {
    const ms = Date.parse(time);
    return Number.isNaN(ms) ? undefined : [ms, 0];
  }
  const [, seconds, fraction = '', zone = 'Z'] = parts;
  const ms = Date.parse(`${seconds}${zone}`);
  if (Number.isNaN(ms)) return undefined;
  return [ms, Number(fraction.padEnd(9, '0').slice(0, 9))];
};
