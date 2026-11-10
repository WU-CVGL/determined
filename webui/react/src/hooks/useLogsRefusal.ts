import { useCallback, useEffect, useRef, useState } from 'react';

import usePageReturns from 'hooks/usePageReturns';

interface LogsRefusal {
  /** Whether the logs are not shared with the viewer: by the capabilities or by a refusal. */
  notShared: boolean;
  /** Records the master's refusal of the logs of the job the hook was given. */
  refuse: () => void;
}

/**
 * The refusal of a job's logs by the master, kept until the capabilities given turn from hiding
 * the logs to showing them, the job changes, or the user comes back to the page while the
 * capabilities show the logs. Capabilities that merely still show the logs do not clear it, so
 * stale ones do not open the logs again and again.
 */
const useLogsRefusal = <Id extends number | string>(
  id: Id | undefined,
  logsHidden: boolean,
): LogsRefusal => {
  const [refusedId, setRefusedId] = useState<Id>();
  const wasHidden = useRef(logsHidden);

  useEffect(() => {
    if (wasHidden.current && !logsHidden) setRefusedId(undefined);
    wasHidden.current = logsHidden;
  }, [logsHidden]);

  useEffect(() => {
    setRefusedId(undefined);
  }, [id]);

  usePageReturns(() => {
    if (!logsHidden) setRefusedId(undefined);
  });

  const refuse = useCallback(() => setRefusedId(id), [id]);

  return { notShared: logsHidden || (id !== undefined && refusedId === id), refuse };
};

export default useLogsRefusal;
