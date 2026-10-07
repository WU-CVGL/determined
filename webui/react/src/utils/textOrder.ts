/*
 * The text order of the Jobs page and of the Active tab of a resource pool: the master's order,
 * which Postgres gives with lower(x COLLATE "C"), x COLLATE "C" and Go with its byte compare.
 * Neither localeCompare nor < give it.
 */

/*
 * UTF-16 code units in the order of the code points they encode: surrogates, which encode the code
 * points above U+FFFF, after U+E000-U+FFFF.
 */
const codeUnitRank = (unit: number): number => {
  if (unit >= 0xd800 && unit <= 0xdfff) return unit + 0x2000;
  if (unit >= 0xe000) return unit - 0x800;
  return unit;
};

/**
 * Compares by code point, as Go compares strings and Postgres under the C collation: both compare
 * UTF-8 bytes, which sort as their code points do. Not by UTF-16 code unit, which puts 😀 (U+1F600)
 * before ｱ (U+FF71).
 */
export const compareCodePoints = (a: string, b: string): number => {
  const length = Math.min(a.length, b.length);
  for (let i = 0; i < length; i++) {
    const unitA = a.charCodeAt(i);
    const unitB = b.charCodeAt(i);
    if (unitA !== unitB) return codeUnitRank(unitA) < codeUnitRank(unitB) ? -1 : 1;
  }
  return Math.sign(a.length - b.length);
};

/** A-Z to a-z, and nothing else: what lower() does under the C collation. */
const foldAZ = (text: string): string => text.replace(/[A-Z]+/g, (upper) => upper.toLowerCase());

/**
 * The order of names, owners and pools: by code point with A-Z folded to a-z, then by code point
 * as is.
 */
export const compareText = (a: string, b: string): number =>
  compareCodePoints(foldAZ(a), foldAZ(b)) || compareCodePoints(a, b);
