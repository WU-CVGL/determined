import Alert from 'hew/Alert';
import { Modal, useModal } from 'hew/Modal';
import Select, { Option } from 'hew/Select';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useState } from 'react';

import { getJobSharing, patchJobSharing } from 'services/api';
import { V1JobContentKind, V1PatchJobSharingRequest } from 'services/api-ts-sdk';
import { JobSharingSettings } from 'types';
import handleError from 'utils/error';
import {
  audienceAllUsers,
  JOB_CONTENT_KINDS,
  JobContentKind,
  masterErrorText,
  sharingAllowed,
} from 'utils/jobPrivacy';

import css from './JobSharingModal.module.scss';

const EVERYONE = 'everyone';
const OWNER_ONLY = 'owner-only';
type Choice = typeof EVERYONE | typeof OWNER_ONLY;

const ROW_LABELS: Record<JobContentKind, string> = {
  [V1JobContentKind.TITLE]: 'Title',
  [V1JobContentKind.LOGS]: 'Logs',
  [V1JobContentKind.CONFIG]: 'Config',
};

const FIELDS = {
  [V1JobContentKind.TITLE]: 'title',
  [V1JobContentKind.LOGS]: 'logs',
  [V1JobContentKind.CONFIG]: 'config',
} as const satisfies Record<JobContentKind, keyof V1PatchJobSharingRequest>;

type Choices = Record<JobContentKind, Choice>;

/** The job's choices: an audience that is not set is a job from before job privacy, public. */
const choicesOf = ({ sharing }: JobSharingSettings): Choices => {
  const choice = (kind: JobContentKind): Choice =>
    audienceAllUsers(sharing[FIELDS[kind]]) === false ? OWNER_ONLY : EVERYONE;
  return {
    [V1JobContentKind.TITLE]: choice(V1JobContentKind.TITLE),
    [V1JobContentKind.LOGS]: choice(V1JobContentKind.LOGS),
    [V1JobContentKind.CONFIG]: choice(V1JobContentKind.CONFIG),
  };
};

interface Props {
  jobId: string;
  /** Called when the modal closes, saved or not. */
  onClose: () => void;
  /** Called after the sharing was saved. */
  onSaved?: () => void;
}

/**
 * A job's own sharing, for its owner and admins: each kind is shared with everyone or only with
 * the owner and admins. The modal is mounted while it is open, and reads the job's sharing then.
 * Saving sends only the kinds that were changed.
 */
const JobSharingModalComponent: React.FC<Props> = ({ jobId, onClose, onSaved }: Props) => {
  const [settings, setSettings] = useState<JobSharingSettings>();
  const [loadError, setLoadError] = useState<string>();
  const [saveError, setSaveError] = useState<string>();
  const [choices, setChoices] = useState<Choices>();

  useEffect(() => {
    const canceler = new AbortController();
    getJobSharing({ jobId }, { signal: canceler.signal })
      .then((response) => {
        setSettings(response);
        setChoices(choicesOf(response));
      })
      .catch((e) => {
        if (canceler.signal.aborted) return;
        setLoadError(masterErrorText(e));
        handleError(e, { silent: true });
      });
    return () => canceler.abort();
  }, [jobId]);

  const handleSubmit = useCallback(async () => {
    if (!settings || !choices) return;
    setSaveError(undefined);
    const saved = choicesOf(settings);
    const request: V1PatchJobSharingRequest = { jobId };
    JOB_CONTENT_KINDS.forEach((kind) => {
      if (choices[kind] !== saved[kind]) {
        request[FIELDS[kind]] = { allUsers: choices[kind] === EVERYONE };
      }
    });
    await patchJobSharing(request);
  }, [choices, jobId, settings]);

  const handleSaveError = useCallback((e: unknown) => {
    setSaveError(masterErrorText(e));
  }, []);

  const handleComplete = useCallback(() => {
    onSaved?.();
    onClose();
  }, [onClose, onSaved]);

  const titleOwnerOnly = choices?.[V1JobContentKind.TITLE] === OWNER_ONLY;

  return (
    <Modal
      cancel
      size="small"
      submit={{
        disabled: !choices,
        handleError: handleSaveError,
        handler: handleSubmit,
        onComplete: handleComplete,
        text: 'Save',
      }}
      title="Sharing"
      onClose={onClose}>
      {loadError ? (
        <Alert message={loadError} type="error" />
      ) : settings && choices ? (
        <div className={css.rows}>
          {JOB_CONTENT_KINDS.map((kind) => (
            <div className={css.row} data-testid={`job-sharing-${FIELDS[kind]}`} key={kind}>
              <span>{ROW_LABELS[kind]}</span>
              <Select
                disabled={kind !== V1JobContentKind.TITLE && titleOwnerOnly}
                searchable={false}
                value={choices[kind]}
                width={140}
                onChange={(value) =>
                  setChoices((prev) => prev && { ...prev, [kind]: value as Choice })
                }>
                <Option disabled={!sharingAllowed(settings.rules, kind)} value={EVERYONE}>
                  Everyone
                </Option>
                <Option value={OWNER_ONLY}>Owner only</Option>
              </Select>
            </div>
          ))}
          {saveError && <Alert message={saveError} type="error" />}
        </div>
      ) : (
        <Spinner center spinning />
      )}
    </Modal>
  );
};

/**
 * The Sharing action of a job: open(jobId) opens the modal, which modal renders while it is open.
 * onSaved runs after a save, e.g. to read the job again.
 */
export const useJobSharingModal = (
  onSaved?: () => void,
): { modal: React.ReactNode; open: (jobId: string) => void } => {
  const { Component, open: openModal } = useModal(JobSharingModalComponent);
  const [jobId, setJobId] = useState<string>();

  const open = useCallback(
    (id: string) => {
      setJobId(id);
      openModal();
    },
    [openModal],
  );
  const close = useCallback(() => setJobId(undefined), []);

  const modal = jobId ? <Component jobId={jobId} onClose={close} onSaved={onSaved} /> : null;
  return { modal, open };
};

export default JobSharingModalComponent;
