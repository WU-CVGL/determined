import Badge from 'hew/Badge';
import { Option } from 'hew/Select';
import React from 'react';

import { ResourcePool } from 'types';
import { isUsablePool } from 'utils/cluster';

import css from './ResourcePoolOption.module.scss';

const RESTRICTED_COLOR = { h: 32, l: 50, s: 90 };

/** The tag of a resource pool that the user may not use. */
export const RestrictedTag: React.FC = () => (
  <Badge backgroundColor={RESTRICTED_COLOR} text="Restricted" />
);

/**
 * The option of a pool picker. A pool that the user may not use is listed, striped and tagged, but
 * cannot be picked. A function, not a component, because Select reads its Option children.
 */
export const resourcePoolOption = (pool: Pick<ResourcePool, 'name' | 'usable'>): JSX.Element => {
  const usable = isUsablePool(pool);
  return (
    <Option
      className={usable ? undefined : css.restricted}
      disabled={!usable}
      key={pool.name}
      value={pool.name}>
      {usable ? (
        pool.name
      ) : (
        <span className={css.label}>
          {pool.name}
          <RestrictedTag />
        </span>
      )}
    </Option>
  );
};
