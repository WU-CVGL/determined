import { Modal } from 'hew/Modal';

import { UNMANAGED_EXPERIMENT_ANNOTATION_MESSAGE } from 'constant';
import { ProjectExperiment } from 'types';
import handleError from 'utils/error';
import { handleTensorBoardError, openTensorBoardSession } from 'utils/tensorBoardSession';

interface Props {
  selectedExperiments: ProjectExperiment[];
  workspaceId?: number;
}

const ExperimentTensorBoardModal = ({ workspaceId, selectedExperiments }: Props): JSX.Element => {
  const handleSubmit = async () => {
    const managedExperimentIds = selectedExperiments
      .filter((exp) => !exp.unmanaged)
      .map((exp) => exp.id);
    try {
      await openTensorBoardSession({ experimentIds: managedExperimentIds, workspaceId });
    } catch (e) {
      handleTensorBoardError(e, 'Unable to View TensorBoard for Selected Experiments');
    }
  };

  return (
    <Modal
      cancel
      size="small"
      submit={{
        handleError,
        handler: handleSubmit,
        text: 'confirm',
      }}
      title="Tensorboard confirmation">
      {UNMANAGED_EXPERIMENT_ANNOTATION_MESSAGE}
    </Modal>
  );
};

export default ExperimentTensorBoardModal;
