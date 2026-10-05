import Button from 'hew/Button';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useState } from 'react';
import { useParams, useSearchParams } from 'react-router-dom';

import Badge, { BadgeType } from 'components/Badge';
import PageMessage from 'components/PageMessage';
import useUI from 'components/ThemeProvider';
import { terminalCommandStates } from 'constants/states';
import { serverAddress } from 'routes/utils';
import { getTask } from 'services/api';
import { CommandState, CommandType } from 'types';
import handleError, { ErrorType } from 'utils/error';
import { capitalize } from 'utils/string';
import {
  getJupyterLabAddress,
  hasJupyterToken,
  NOTEBOOK_ACCESS_DENIED,
  WaitStatus,
} from 'utils/wait';

import css from './Wait.module.scss';

type Params = {
  taskId: string;
  taskType: string;
};

const Wait: React.FC = () => {
  const {
    actions: { showChrome, hideChrome },
  } = useUI();
  const [searchParams] = useSearchParams();
  const { taskType } = useParams<Params>();
  const [waitStatus, setWaitStatus] = useState<WaitStatus>();
  const [accessDenied, setAccessDenied] = useState(false);
  const [addressFailed, setAddressFailed] = useState(false);
  const serviceAddr = searchParams.get('serviceAddr');
  const taskId = serviceAddr ? (serviceAddr.match(/[0-f-]+/) || ' ')[0] : undefined;

  const capitalizedTaskType = capitalize(taskType ?? '');
  const isLoading =
    !accessDenied &&
    !addressFailed &&
    (!waitStatus || !terminalCommandStates.has(waitStatus.state));

  let message = `Waiting for ${capitalizedTaskType} ...`;
  if (!serviceAddr) {
    message = 'Missing required parameters.';
  } else if (accessDenied) {
    message = NOTEBOOK_ACCESS_DENIED;
  } else if (addressFailed) {
    message = `${capitalizedTaskType} is ready, but its address could not be loaded.`;
  } else if (waitStatus && terminalCommandStates.has(waitStatus.state)) {
    message = `${capitalizedTaskType} has been terminated.`;
  } else if (
    capitalizedTaskType === 'Tensor-board' &&
    waitStatus &&
    waitStatus?.state === CommandState.Waiting
  ) {
    message = `Waiting for ${capitalizedTaskType} metrics step to be completed.`;
  }

  useEffect(() => {
    hideChrome();
    return showChrome;
  }, [hideChrome, showChrome]);

  const handleTaskError = (e: Error) => {
    handleError(e, {
      publicMessage:
        'Failed while waiting for command to be ready. This may be caused by not having related permissions',
      silent: false,
      type: ErrorType.Server,
    });
  };

  const openService = useCallback(async () => {
    if (!serviceAddr || !taskId) return;
    let address: string | undefined = serviceAddr;
    // Listings never include a notebook's Jupyter token; only its owner and administrators
    // can read it, and JupyterLab does not open without it.
    if (taskType === CommandType.JupyterLab && !hasJupyterToken(serviceAddr)) {
      try {
        address = await getJupyterLabAddress(taskId);
      } catch (e) {
        handleError(e as Error, {
          publicMessage: 'Failed to load the notebook address.',
          silent: false,
          type: ErrorType.Server,
        });
        setAddressFailed(true);
        return;
      }
    }
    setAddressFailed(false);
    if (address) {
      window.location.assign(serverAddress(address));
    } else {
      setAccessDenied(true);
    }
  }, [serviceAddr, taskId, taskType]);

  useEffect(() => {
    if (!serviceAddr || !taskId) return;
    const ival = setInterval(async () => {
      try {
        const response = await getTask({ taskId });
        if (!response?.allocations?.length) {
          return;
        }
        const lastRun = response.allocations[0];
        if (!lastRun) {
          return;
        }
        if (CommandState.Terminated === lastRun.state) {
          clearInterval(ival);
        } else if (lastRun.isReady) {
          clearInterval(ival);
          await openService();
        }
        // TODO: use task.endTime to determine if the task is terminated.
        setWaitStatus(lastRun);
      } catch (e) {
        handleTaskError(e as Error);
      }
    }, 1000);
    return () => clearInterval(ival);
  }, [openService, serviceAddr, taskId]);

  return (
    <PageMessage title={capitalizedTaskType}>
      <div className={css.base}>
        <div className={css.message}>{message}</div>
        {waitStatus && (
          <div className={css.state}>
            <Badge state={waitStatus?.state} type={BadgeType.State} />
          </div>
        )}
        {addressFailed && <Button onClick={openService}>Try Again</Button>}
        <Spinner spinning={isLoading} />
      </div>
    </PageMessage>
  );
};

export default Wait;
