import Button from 'hew/Button';
import Tooltip from 'hew/Tooltip';
import React, { useCallback } from 'react';

import { useLaunchForm } from 'hooks/useLaunchForm';
import { CommandType, Workspace } from 'types';

interface Props {
  enabled?: boolean;
  workspace?: Workspace;
}

/** Opens the launch form with Shell selected. */
const ShellButton: React.FC<Props> = ({ enabled, workspace }: Props) => {
  const { launchFormModals, openLaunchForm } = useLaunchForm({ workspace });

  const handleClick = useCallback(() => openLaunchForm(CommandType.Shell), [openLaunchForm]);

  return (
    <div data-testid="shell-button">
      {enabled ? (
        <>
          <Button onClick={handleClick}>Launch Shell</Button>
          {launchFormModals}
        </>
      ) : (
        <Tooltip content="You do not have permission to launch a shell" placement="leftBottom">
          <div>
            <Button disabled>Launch Shell</Button>
          </div>
        </Tooltip>
      )}
    </div>
  );
};

export default ShellButton;
