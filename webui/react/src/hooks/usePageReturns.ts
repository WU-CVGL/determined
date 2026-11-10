import { useEffect, useRef, useState } from 'react';

import useUI from 'components/ThemeProvider';

/**
 * How many times the user has come back to the page after it was hidden, e.g. to repeat a lookup
 * whose answer may have changed meanwhile. `onReturn` also runs at each return.
 */
const usePageReturns = (onReturn?: () => void): number => {
  const { ui } = useUI();
  const [returns, setReturns] = useState(0);
  const wasHidden = useRef(ui.isPageHidden);
  const handler = useRef(onReturn);

  useEffect(() => {
    handler.current = onReturn;
  });

  useEffect(() => {
    if (wasHidden.current && !ui.isPageHidden) {
      setReturns((count) => count + 1);
      handler.current?.();
    }
    wasHidden.current = ui.isPageHidden;
  }, [ui.isPageHidden]);

  return returns;
};

export default usePageReturns;
