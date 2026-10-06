import { matchesShortcut } from 'hew/InputShortcut';
import React, { useEffect } from 'react';

import shortCutSettingsConfig, {
  Settings as ShortcutSettings,
} from 'components/UserSettings.settings';
import { keyEmitter, KeyEvent } from 'hooks/useKeyTracker';
import { useLaunchForm } from 'hooks/useLaunchForm';
import { useSettings } from 'hooks/useSettings';
import { CommandType, Workspace } from 'types';

interface Props {
  enabled?: boolean;
  workspace?: Workspace;
}

/** The JupyterLab keyboard shortcut: opens the launch form with JupyterLab selected. */
const JupyterLabGlobal: React.FC<Props> = ({ enabled, workspace }) => {
  const { launchFormModals, openLaunchForm } = useLaunchForm({ workspace });
  const {
    settings: { jupyterLab: jupyterLabShortcut },
  } = useSettings<ShortcutSettings>(shortCutSettingsConfig);

  useEffect(() => {
    const keyDownListener = (e: KeyboardEvent) => {
      if (matchesShortcut(e, jupyterLabShortcut)) {
        openLaunchForm(CommandType.JupyterLab);
      }
    };

    if (enabled) keyEmitter.on(KeyEvent.KeyDown, keyDownListener);

    return () => {
      keyEmitter.off(KeyEvent.KeyDown, keyDownListener);
    };
  }, [openLaunchForm, jupyterLabShortcut, enabled]);

  return <>{launchFormModals}</>;
};

export default JupyterLabGlobal;
