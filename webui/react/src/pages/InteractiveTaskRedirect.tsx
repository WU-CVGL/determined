import React from 'react';
import { Navigate, useLocation, useParams } from 'react-router-dom';

import { paths } from 'routes/utils';
import { InteractiveTaskTab } from 'types';

interface Props {
  /** The tab to land on, e.g. Terminal for the old terminal page. */
  tab?: InteractiveTaskTab;
}

type Params = {
  taskId: string;
};

/**
 * Sends the old terminal page, /shells/:taskId/terminal, to the task page by ID.
 */
const InteractiveTaskRedirect: React.FC<Props> = ({ tab }: Props) => {
  const { taskId = '' } = useParams<Params>();
  const location = useLocation();
  const search = new URLSearchParams(location.search);
  return (
    <Navigate
      replace
      to={paths.interactiveTask(taskId, {
        currentSlotsExceeded: search.get('currentSlotsExceeded') === 'true',
        tab,
      })}
    />
  );
};

export default InteractiveTaskRedirect;
