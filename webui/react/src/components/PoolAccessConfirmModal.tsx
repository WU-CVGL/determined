import Button from 'hew/Button';
import { Modal } from 'hew/Modal';
import Row from 'hew/Row';
import React, { useCallback, useState } from 'react';

import PoolAccessResults, { PoolAccessResultAction } from 'components/PoolAccessResults';
import { PoolAccessResult } from 'utils/resourcePoolAccess';

interface Props {
  action: PoolAccessResultAction;
  closeModal: () => void;
  content: React.ReactNode;
  danger?: boolean;
  okText: string;
  /** Sends the change. It goes on, and its results are kept, when the modal is closed. */
  run: () => Promise<PoolAccessResult[]>;
  title: string;
}

/**
 * Asks before a change of pool access and then shows what the master answered for each pool. The
 * modal stays open with the results, so failures and warnings are not lost in a toast.
 */
const PoolAccessConfirmModalComponent: React.FC<Props> = ({
  action,
  closeModal,
  content,
  danger,
  okText,
  run,
  title,
}: Props) => {
  const [isApplying, setIsApplying] = useState(false);
  const [results, setResults] = useState<PoolAccessResult[]>();

  const handleApply = useCallback(async () => {
    setIsApplying(true);
    try {
      setResults(await run());
    } finally {
      setIsApplying(false);
    }
  }, [run]);

  const footer = results ? (
    <Row>
      <Button type="primary" onClick={closeModal}>
        Close
      </Button>
    </Row>
  ) : (
    <Row>
      <Button disabled={isApplying} onClick={closeModal}>
        Cancel
      </Button>
      <Button danger={danger} loading={isApplying} type="primary" onClick={handleApply}>
        {okText}
      </Button>
    </Row>
  );

  return (
    <Modal footer={footer} size="medium" title={title} onClose={closeModal}>
      {results ? <PoolAccessResults action={action} results={results} /> : content}
    </Modal>
  );
};

export default PoolAccessConfirmModalComponent;
