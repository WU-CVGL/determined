import { Modal } from 'hew/Modal';
import { ShirtSize } from 'hew/Theme';
import React from 'react';

import Grid from 'components/Grid';
import Link from 'components/Link';
import { pluralizer } from 'utils/string';

import css from './TensorBoardSourcesModal.module.scss';

export interface TensorBoardSource {
  id: number;
  path: string;
  type: string;
}

interface Props {
  onClose: () => void;
  sources: TensorBoardSource[];
}

/** Links to the experiments and trials that a TensorBoard shows. */
const TensorBoardSourcesModal: React.FC<Props> = ({ onClose, sources }: Props) => {
  return (
    <Modal
      size="medium"
      submit={{
        handleError: () => {},
        handler: onClose,
        text: 'Close',
      }}
      title={`${sources.length} TensorBoard ${pluralizer(sources.length, 'Source')}`}
      onClose={onClose}>
      <div className={css.sourceLinks}>
        <Grid gap={ShirtSize.Medium} minItemWidth={120}>
          {sources.map((source) => (
            <Link key={`${source.type}-${source.id}`} path={source.path}>
              {source.type} {source.id}
            </Link>
          ))}
        </Grid>
      </div>
    </Modal>
  );
};

export default TensorBoardSourcesModal;
