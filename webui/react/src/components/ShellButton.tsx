import Button from 'hew/Button';
import Tooltip from 'hew/Tooltip';
import React, { useCallback } from 'react';

import { useLaunchForm } from 'hooks/useLaunchForm';
import { Workspace } from 'types';

interface Props {
  enabled?: boolean;
  workspace?: Workspace;
}

/** Opens the shell launch form. */
const ShellButton: React.FC<Props> = ({ enabled, workspace }: Props) => {
  const { launchFormModals, openLaunchForm } = useLaunchForm({ workspace });

  const handleClick = useCallback(() => openLaunchForm(), [openLaunchForm]);

  return (
    <div data-testid="shell-button">
      {enabled ? (
        <>
          <Button onClick={handleClick}>Launch Interactive Task</Button>
          {launchFormModals}
        </>
      ) : (
        <Tooltip
          content="You do not have permission to launch an interactive task"
          placement="leftBottom">
          <div>
            <Button disabled>Launch Interactive Task</Button>
          </div>
        </Tooltip>
      )}
    </div>
  );
};

export default ShellButton;
