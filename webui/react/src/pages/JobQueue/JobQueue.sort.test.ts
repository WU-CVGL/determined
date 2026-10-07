import { compareText } from './JobQueue.sort';

const sorted = (texts: string[]) => [...texts].sort(compareText);

describe('compareText', () => {
  it('folds A to Z only, then puts capitals first', () => {
    expect(sorted(['b', 'B', 'a', 'A'])).toEqual(['A', 'a', 'B', 'b']);
    expect(sorted(['z', 'é', 'É', 'e'])).toEqual(['e', 'z', 'É', 'é']);
  });

  it('orders by code point: digits, then underscores, then letters', () => {
    expect(sorted(['ab', 'a_b', 'aB', 'a1'])).toEqual(['a1', 'a_b', 'aB', 'ab']);
    expect(sorted(['alpha_2', 'alpha2', 'Alpha'])).toEqual(['Alpha', 'alpha2', 'alpha_2']);
  });

  it('orders by code point beyond the Basic Multilingual Plane, not by UTF-16 unit', () => {
    expect(sorted(['\u{1F600}', '～'])).toEqual(['～', '\u{1F600}']);
  });

  it('puts the empty text and a prefix first, and finds equal texts equal', () => {
    expect(sorted(['a', '', 'a0'])).toEqual(['', 'a', 'a0']);
    expect(compareText('job', 'job')).toBe(0);
  });
});
