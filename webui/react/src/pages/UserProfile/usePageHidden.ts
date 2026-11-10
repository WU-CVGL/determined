import { useEffect, useState } from 'react';

/** Whether the page is hidden, from the document itself: also for a page opened hidden. */
const usePageHidden = (): boolean => {
  const [hidden, setHidden] = useState(() => document.hidden);
  useEffect(() => {
    const update = () => setHidden(document.hidden);
    update();
    document.addEventListener('visibilitychange', update);
    return () => document.removeEventListener('visibilitychange', update);
  }, []);
  return hidden;
};

export default usePageHidden;
