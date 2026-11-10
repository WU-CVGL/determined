import { alertAction, parseIds, visitAction } from 'omnibar/tree-extension/trees/actions';
import { Children, TreeNode } from 'omnibar/tree-extension/types';
import { paths } from 'routes/utils';
import { getExperimentDetails, getTrialDetails } from 'services/api';

const locations: TreeNode[] = [
  {
    aliases: ['agents', 'resourcePools'],
    onAction: visitAction(paths.clusters()),
    title: 'cluster',
  },
  {
    label: 'experiment <id>',
    onCustomInput: (inp: string): Children => {
      const onAction = async () => {
        const id = parseIds(inp)[0];
        try {
          await getExperimentDetails({ id });
          visitAction(paths.experimentDetails(id))();
        } catch {
          alertAction(`Invalid experiment ID ${id}`)();
        }
      };

      const label = inp === '' ? '<id>' : inp;
      return [{ label, onAction, title: inp }];
    },
    title: 'experiment',
  },
  {
    label: 'trial <id>',
    onCustomInput: (inp: string): Children => {
      const onAction = async () => {
        const id = parseIds(inp)[0];
        try {
          const trial = await getTrialDetails({ id });
          visitAction(paths.trialDetails(trial.id, trial.experimentId))();
        } catch {
          alertAction(`Invalid trial ID ${id}`)();
        }
      };

      // we could generate this `<id>` arg label and the label for the
      // parent node together instead of separately.
      const label = inp === '' ? '<id>' : inp;
      return [{ label, onAction, title: inp }];
    },
    title: 'trial',
  },
  {
    aliases: ['commands', 'shells'],
    onAction: visitAction(paths.taskList()),
    title: 'tasks',
  },
  {
    onAction: visitAction(paths.clusterLogs()),
    title: 'clusterLogs',
  },
];

export default locations;
