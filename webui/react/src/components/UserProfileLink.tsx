import React from 'react';

import Link from 'components/Link';
import { handlePath, paths } from 'routes/utils';

interface Props {
  /** What the link shows. */
  children: React.ReactNode;
  /** The user's name, the link's accessible name. */
  name: string;
  userId: number;
}

/** A link to a user's page; its click does not reach the row or card around it. */
const UserProfileLink: React.FC<Props> = ({ children, name, userId }: Props) => {
  const path = paths.userProfile(userId);
  return (
    <Link
      label={name}
      path={path}
      onClick={(e) => {
        e.stopPropagation();
        handlePath(e, { path });
      }}>
      {children}
    </Link>
  );
};

export default UserProfileLink;
