import Icon from 'hew/Icon';
import React from 'react';

import { PoolAccessResult } from 'utils/resourcePoolAccess';
import { pluralizer } from 'utils/string';

import css from './PoolAccessResults.module.scss';

export type PoolAccessResultAction = 'grant' | 'revoke' | 'restrict' | 'public';

const usernames = (count: number): string => `${count} ${pluralizer(count, 'username')}`;

/**
 * A grant or revoke counts the usernames sent, not the users whose access changed: the master
 * does not say which users already had a grant or had none to revoke.
 */
const doneText = (action: PoolAccessResultAction, result: PoolAccessResult): string => {
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

const failedText = (result: PoolAccessResult): string => {
  if (result.requests <= 1 || result.totalUsernames === 0) return `failed: ${result.error}`;
  return (
    `failed: ${result.error}. ${result.appliedRequests} of ${result.requests} requests were ` +
    `applied (${result.appliedUsernames} of ${usernames(result.totalUsernames)}); nothing was ` +
    'retried'
  );
};

interface Props {
  action: PoolAccessResultAction;
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
                <strong>{result.poolName}</strong>:{' '}
                {result.ok ? doneText(action, result) : failedText(result)}
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
      {(action === 'grant' || action === 'revoke') && (
        <p className={css.note}>
          A user who already had a grant, or had none to revoke, is counted but unchanged.
        </p>
      )}
    </div>
  );
};

export default PoolAccessResults;
