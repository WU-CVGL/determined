import { useCallback, useEffect, useRef, useState } from 'react';

import usePageReturns from 'hooks/usePageReturns';

interface Refusal {
  /** Whether the content is not shared with the viewer: by the capabilities or by a refusal. */
  notShared: boolean;
  /** Records the master's refusal of the content of the job the hook was given. */
  refuse: () => void;
}

/**
 * The refusal of a job's content, e.g. its logs or config, by the master, kept until the
 * capabilities given turn from hiding the content to showing it, the job changes, or the user
 * comes back to the page while the capabilities show the content. Capabilities that merely still
 * show the content do not clear it, so stale ones do not read the content again and again.
 */
const useRefusal = <Id extends number | string>(id: Id | undefined, hidden: boolean): Refusal => {
  const [refusedId, setRefusedId] = useState<Id>();
  const wasHidden = useRef(hidden);

  useEffect(() => {
    if (wasHidden.current && !hidden) setRefusedId(undefined);
    wasHidden.current = hidden;
  }, [hidden]);

  useEffect(() => {
    setRefusedId(undefined);
  }, [id]);

  usePageReturns(() => {
    if (!hidden) setRefusedId(undefined);
  });

  const refuse = useCallback(() => setRefusedId(id), [id]);

  return { notShared: hidden || (id !== undefined && refusedId === id), refuse };
};

export default useRefusal;
