import Button from 'hew/Button';
import { shortcutToString } from 'hew/InputShortcut';
import Tooltip from 'hew/Tooltip';
import React, { useCallback } from 'react';

import shortCutSettingsConfig, {
  Settings as ShortcutSettings,
} from 'components/UserSettings.settings';
import { useLaunchForm } from 'hooks/useLaunchForm';
import { useSettings } from 'hooks/useSettings';
import { CommandType, Workspace } from 'types';
interface Props {
  enabled?: boolean;
  workspace?: Workspace;
}

/** Opens the launch form with JupyterLab selected. */
const JupyterLabButton: React.FC<Props> = ({ enabled, workspace }: Props) => {
  const { launchFormModals, openLaunchForm } = useLaunchForm({ workspace });
  const {
    settings: { jupyterLab: jupyterLabShortcut },
  } = useSettings<ShortcutSettings>(shortCutSettingsConfig);

  const handleClick = useCallback(() => openLaunchForm(CommandType.JupyterLab), [openLaunchForm]);

  return (
    <div data-testid="jupyter-lab-button">
      {enabled ? (
        <>
          <Tooltip content={shortcutToString(jupyterLabShortcut)}>
            <Button onClick={handleClick}>Launch JupyterLab</Button>
          </Tooltip>
          {launchFormModals}
        </>
      ) : (
        <Tooltip content="You do not have permission to launch JupyterLab" placement="leftBottom">
          <div>
            <Button disabled>Launch JupyterLab</Button>
          </div>
        </Tooltip>
      )}
    </div>
  );
};

export default JupyterLabButton;
