import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import React, { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';

import useUI from 'components/ThemeProvider';
import { errorText, taskServiceUrl } from 'services/taskService';
import { replaceWithExternalUrl } from 'utils/routes';

type Params = {
  service: string;
  taskId: string;
};

/**
 * Opens a service of a task once the browser is logged in: the credential is read as the
 * browser's user, the browser logs in to the service when it has a login form, and the page is
 * replaced by the service.
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
    taskServiceUrl(taskId, serviceName)
      .then((url) => {
        if (!cancelled) replaceWithExternalUrl(url);
      })
      .catch((e) => {
        if (!cancelled) setError(errorText(e));
      });
    return () => {
      cancelled = true;
    };
  }, [serviceName, taskId]);

  return error ? <Message title={error} /> : <Spinner center spinning />;
};

export default TaskServiceOpen;
