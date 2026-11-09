import Link from 'hew/Link';
import React from 'react';

import { handlePath, linkPath, paths } from 'routes/utils';

interface Props {
  children?: React.ReactNode;
  userId: number;
}

/** A link to a user's page; its click does not reach the row or card around it. */
const UserProfileLink: React.FC<Props> = ({ children, userId }: Props) => {
  const path = paths.userProfile(userId);
  return (
    <Link href={linkPath(path)} onClick={(e) => handlePath(e, { path })}>
      {children}
    </Link>
  );
};

export default UserProfileLink;
