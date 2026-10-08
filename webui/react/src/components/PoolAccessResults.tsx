import Icon from 'hew/Icon';
import React from 'react';

import { PoolAccessAction, PoolAccessResult } from 'utils/resourcePoolAccess';
import { pluralizer } from 'utils/string';

import css from './PoolAccessResults.module.scss';

/**
 * Sends a change and answers with its results, or with undefined, sending nothing, while another
 * change runs. The Pool Access tab passes it to its dialogs: one change runs at a time, and it
 * keeps its results when its dialog is closed before the master answered. run gets the signal
 * that ends the change at sign-out.
 */
export type PoolAccessRunner = (
  action: PoolAccessAction,
  run: (signal: AbortSignal) => Promise<PoolAccessResult[]>,
) => Promise<PoolAccessResult[] | undefined>;

export const CHANGE_RUNNING_MESSAGE = 'Another change is running.';
export const changeStoppedMessage = (error: string): string => `The change stopped: ${error}.`;
export const UNCONFIRMED_NOTE = 'A failed request may still have been applied.';

const usernames = (count: number): string => `${count} ${pluralizer(count, 'username')}`;

/**
 * A grant or revoke counts the usernames sent, not the users whose access changed: the master
 * does not say which users already had a grant or had none to revoke.
 */
const doneText = (action: PoolAccessAction, result: PoolAccessResult): string => {
  switch (action) {
    case 'grant':
      return `grant applied for ${usernames(result.totalUsernames)}`;
    case 'revoke':
      return `revoke applied for ${usernames(result.totalUsernames)}`;
    case 'restrict':
      return 'restricted';
    case 'public':
      return 'made public';
  }
};

/** A failed request may have been applied too: only the confirmed requests are counted. */
const failedText = (result: PoolAccessResult): string => {
  if (result.requests <= 1 || result.totalUsernames === 0) return `failed: ${result.error}`;
  return (
    `failed: ${result.error}. ${result.confirmedRequests} of ${result.requests} requests ` +
    `confirmed (${result.confirmedUsernames} of ${usernames(result.totalUsernames)}); nothing ` +
    'was retried'
  );
};

/** What the master answered for one pool, as the results list it. */
export const poolAccessResultText = (action: PoolAccessAction, result: PoolAccessResult): string =>
  result.ok ? doneText(action, result) : failedText(result);

interface Props {
  action: PoolAccessAction;
  results: PoolAccessResult[];
}

/** What the master answered for each pool of a change, with its warnings. */
const PoolAccessResults: React.FC<Props> = ({ action, results }: Props) => {
  const failed = results.filter((result) => !result.ok).length;
  return (
    <div className={css.base} data-testid="pool-access-results">
      <p>
        {failed === 0
          ? `Done for ${results.length} ${pluralizer(results.length, 'pool')}.`
          : `${failed} of ${results.length} ${pluralizer(results.length, 'pool')} failed.`}
      </p>
      <ul className={css.list}>
        {results.map((result) => (
          <li data-testid={`pool-access-result-${result.poolName}`} key={result.poolName}>
            <div className={css.line}>
              <Icon
                color={result.ok ? 'success' : 'error'}
                name={result.ok ? 'checkmark' : 'error'}
                size="small"
                title={result.ok ? 'Succeeded' : 'Failed'}
              />
              <span>
                <strong>{result.poolName}</strong>: {poolAccessResultText(action, result)}
              </span>
            </div>
            {result.warnings.length > 0 && (
              <ul className={css.warnings}>
                {result.warnings.map((warning) => (
                  <li key={warning}>{warning}</li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ul>
      {failed > 0 && <p className={css.note}>{UNCONFIRMED_NOTE}</p>}
      {(action === 'grant' || action === 'revoke') && (
        <p className={css.note}>
          A user who already had a grant, or had none to revoke, is counted but unchanged.
        </p>
      )}
    </div>
  );
};

export default PoolAccessResults;
