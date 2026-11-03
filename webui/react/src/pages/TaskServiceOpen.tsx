import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import React, { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';

import useUI from 'components/ThemeProvider';
import { serverAddress } from 'routes/utils';
import { getTaskServiceCredential, getTaskServices } from 'services/api';
import { TaskServiceProtocol, V1TaskServiceState } from 'services/api-ts-sdk';
import { isDetError } from 'utils/error';
import { replaceWithExternalUrl } from 'utils/routes';

type Params = {
  service: string;
  taskId: string;
};

const errorText = (e: unknown): string => {
  if (isDetError(e)) return e.publicMessage || e.message;
  return e instanceof Error ? e.message : String(e);
};

/**
 * Opens a service of a task once the browser is logged in: the credential is read as the
 * browser's user, and the page is replaced by the service.
 */
const TaskServiceOpen: React.FC = () => {
  const { service: serviceName = '', taskId = '' } = useParams<Params>();
  const { actions: uiActions } = useUI();
  const [error, setError] = useState<string>();

  useEffect(() => {
    uiActions.hideChrome();
    return uiActions.showChrome;
  }, [uiActions]);

  useEffect(() => {
    let cancelled = false;
    const open = async (): Promise<void> => {
      try {
        const { services } = await getTaskServices({ taskId });
        const service = services.find((s) => s.name === serviceName);
        if (!service) throw new Error(`No service ${serviceName}`);
        if (service.protocol === TaskServiceProtocol.TCP) {
          throw new Error(`${serviceName}: TCP`);
        }
        if (service.state !== V1TaskServiceState.READY) {
          throw new Error(`${serviceName}: ${service.state.replace('STATE_', '')}`);
        }
        const credential = await getTaskServiceCredential({ service: serviceName, taskId });
        if (!cancelled) replaceWithExternalUrl(serverAddress(credential.url || service.url));
      } catch (e) {
        if (!cancelled) setError(errorText(e));
      }
    };
    void open();
    return () => {
      cancelled = true;
    };
  }, [serviceName, taskId]);

  return error ? <Message title={error} /> : <Spinner center spinning />;
};

export default TaskServiceOpen;
