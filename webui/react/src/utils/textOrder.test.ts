import fixture from 'fixtures/jobsSortOrder.json';

import { compareCodePoints, compareText } from './textOrder';

const order = (texts: string[]) => [...texts].sort(compareText);

describe('compareCodePoints', () => {
  it('compares by code point, not by UTF-16 unit', () => {
    // 😀 (U+1F600) is D83D DE00 in UTF-16, before ～ (U+FF5E) and ｱ (U+FF71) by unit.
    expect('😀' < 'ｱ').toBe(true);
    expect(['ｱ', '😀', '～'].sort(compareCodePoints)).toEqual(['～', 'ｱ', '😀']);
    expect(compareCodePoints('\u{1F600}', '\u{1F601}')).toBe(-1);
    expect(compareCodePoints('\u{10000}', '￿')).toBe(1);
  });

  it('puts a prefix first and finds equal texts equal', () => {
    expect(compareCodePoints('a', 'a😀')).toBe(-1);
    expect(compareCodePoints('a😀', 'a')).toBe(1);
    expect(compareCodePoints('', 'a')).toBe(-1);
    expect(compareCodePoints('job', 'job')).toBe(0);
  });
});

describe('compareText', () => {
  it('folds A-Z only, then puts capitals first', () => {
    expect(order(['beta', 'Beta', 'alpha'])).toEqual(['alpha', 'Beta', 'beta']);
    expect(order(['b', 'B', 'a', 'A'])).toEqual(['A', 'a', 'B', 'b']);
    expect(order(['é', 'É', 'e', 'f'])).toEqual(['e', 'f', 'É', 'é']);
    // KELVIN SIGN lowercases to k in JavaScript, but not under Postgres's C collation.
    expect(order(['K', 'l', 'k'])).toEqual(['k', 'l', 'K']);
  });

  it('puts underscores, digits and spaces where their code points are', () => {
    expect(order(['ab', 'a_b', 'a-b', '_x', '48c', '128c', ' lead'])).toEqual([
      ' lead',
      '128c',
      '48c',
      '_x',
      'a-b',
      'a_b',
      'ab',
    ]);
    expect(order(['ab', 'a_b', 'aB', 'a1'])).toEqual(['a1', 'a_b', 'aB', 'ab']);
    expect(order(['alpha_2', 'alpha2', 'Alpha'])).toEqual(['Alpha', 'alpha2', 'alpha_2']);
  });

  it('puts the empty text and a prefix first, and finds equal texts equal', () => {
    expect(order(['a', '', 'a0'])).toEqual(['', 'a', 'a0']);
    expect(compareText('job', 'job')).toBe(0);
  });

  /*
   * The texts of the Jobs page's shared fixture, which the master's tests check against Postgres
   * and Go, in the order of each text sort. A missing owner or pool is left out: where it goes is
   * each page's own.
   */
  type TextKey = 'name' | 'owner' | 'pool';
  const rows = fixture.rows as Record<TextKey | 'id', string | null>[];
  const orders = fixture.orders as Record<TextKey, { ascend: string[]; descend: string[] }>;
  const fixtureTexts = (key: TextKey, direction: 'ascend' | 'descend'): string[] =>
    orders[key][direction].flatMap((id) => {
      const text = rows.find((row) => row.id === id)?.[key];
      return text ? [text] : [];
    });

  it.each<TextKey>(['name', 'owner', 'pool'])(
    'orders the shared fixture by %s as the master does',
    (key) => {
      const ascending = fixtureTexts(key, 'ascend');
      expect([...ascending].reverse().sort(compareText)).toEqual(ascending);
      expect([...ascending].sort((a, b) => compareText(b, a))).toEqual(
        fixtureTexts(key, 'descend'),
      );
      ascending.slice(1).forEach((text, i) => {
        expect(compareText(ascending[i], text)).toBe(ascending[i] === text ? 0 : -1);
      });
    },
  );
});
