import Alert from 'hew/Alert';
import Select, { Option } from 'hew/Select';
import Spinner from 'hew/Spinner';
import Toggle from 'hew/Toggle';
import React, { useCallback, useEffect, useState } from 'react';

import { getJobPrivacyPolicy, getMyJobPrivacy, patchMyJobPrivacy } from 'services/api';
import {
  V1JobContentKind,
  V1JobPrivacyRule,
  V1PatchMyJobPrivacyRequest,
  V1UserJobPrivacy,
} from 'services/api-ts-sdk';
import handleError from 'utils/error';
import { audienceAllUsers, masterErrorText, sharingAllowed } from 'utils/jobPrivacy';

import css from './UserSettings.module.scss';

const DEFAULT = 'default';
const EVERYONE = 'everyone';
const ONLY_ME = 'only-me';
type NewJobChoice = typeof DEFAULT | typeof EVERYONE | typeof ONLY_ME;

const newJobChoice = (allUsers?: boolean): NewJobChoice =>
  allUsers === undefined ? DEFAULT : allUsers ? EVERYONE : ONLY_ME;

/** The change of a new-job default: "Default" goes back to the cluster's. */
const newJobPatch = (
  choice: NewJobChoice,
  audienceField: 'newJobTitles' | 'newJobLogs',
  resetField: 'resetNewJobTitles' | 'resetNewJobLogs',
): V1PatchMyJobPrivacyRequest =>
  choice === DEFAULT
    ? { [resetField]: true }
    : { [audienceField]: { allUsers: choice === EVERYONE } };

interface KindSettingsProps {
  allowed: boolean;
  disabled: boolean;
  newJobLabel: string;
  newJobs: NewJobChoice;
  onNewJobsChange: (choice: NewJobChoice) => void;
  onShareChange: (share: boolean) => void;
  share: boolean;
  switchLabel: string;
  testId: string;
}

/** A switch and a new-job default of one kind; both are off and disabled when it is forbidden. */
const KindSettings: React.FC<KindSettingsProps> = ({
  allowed,
  disabled,
  newJobLabel,
  newJobs,
  onNewJobsChange,
  onShareChange,
  share,
  switchLabel,
  testId,
}: KindSettingsProps) => (
  <>
    <div data-testid={`${testId}-switch`}>
      <Toggle
        checked={allowed && share}
        disabled={!allowed || disabled}
        label={switchLabel}
        onChange={onShareChange}
      />
    </div>
    <div data-testid={`${testId}-new-jobs`}>
      <Select
        disabled={!allowed || disabled}
        label={newJobLabel}
        searchable={false}
        value={newJobs}
        width={160}
        onChange={(value) => onNewJobsChange(value as NewJobChoice)}>
        <Option value={DEFAULT}>Default</Option>
        <Option value={EVERYONE}>Everyone</Option>
        <Option value={ONLY_ME}>Only me</Option>
      </Select>
    </div>
  </>
);

/**
 * The user's job privacy: whether the titles and the logs of their jobs are shared, and the
 * sharing of their new jobs. Each change is saved at once and sends only that change.
 */
const JobPrivacySettings: React.FC = () => {
  const [rules, setRules] = useState<V1JobPrivacyRule[]>();
  const [privacy, setPrivacy] = useState<V1UserJobPrivacy>();
  const [loadError, setLoadError] = useState<string>();
  const [saveError, setSaveError] = useState<string>();
  const [isSaving, setIsSaving] = useState(false);

  useEffect(() => {
    const canceler = new AbortController();
    Promise.all([
      getJobPrivacyPolicy({}, { signal: canceler.signal }),
      getMyJobPrivacy({}, { signal: canceler.signal }),
    ])
      .then(([policy, mine]) => {
        setRules(policy);
        setPrivacy(mine);
      })
      .catch((e) => {
        if (canceler.signal.aborted) return;
        setLoadError(masterErrorText(e));
        handleError(e, { silent: true });
      });
    return () => canceler.abort();
  }, []);

  const save = useCallback(async (patch: V1PatchMyJobPrivacyRequest) => {
    setIsSaving(true);
    setSaveError(undefined);
    try {
      setPrivacy(await patchMyJobPrivacy(patch));
    } catch (e) {
      setSaveError(masterErrorText(e));
      handleError(e, { silent: true });
    } finally {
      setIsSaving(false);
    }
  }, []);

  if (loadError) return <Alert message={loadError} type="error" />;
  if (!rules || !privacy) return <Spinner spinning />;

  return (
    <div className={css.section}>
      <KindSettings
        allowed={sharingAllowed(rules, V1JobContentKind.TITLE)}
        disabled={isSaving}
        newJobLabel="New job titles"
        newJobs={newJobChoice(audienceAllUsers(privacy.newJobTitles))}
        share={privacy.shareTitles}
        switchLabel="Share titles"
        testId="job-privacy-titles"
        onNewJobsChange={(choice) => save(newJobPatch(choice, 'newJobTitles', 'resetNewJobTitles'))}
        onShareChange={(share) => save({ shareTitles: share })}
      />
      <KindSettings
        allowed={sharingAllowed(rules, V1JobContentKind.LOGS)}
        disabled={isSaving}
        newJobLabel="New job logs"
        newJobs={newJobChoice(audienceAllUsers(privacy.newJobLogs))}
        share={privacy.shareLogs}
        switchLabel="Share logs"
        testId="job-privacy-logs"
        onNewJobsChange={(choice) => save(newJobPatch(choice, 'newJobLogs', 'resetNewJobLogs'))}
        onShareChange={(share) => save({ shareLogs: share })}
      />
      {saveError && <Alert message={saveError} type="error" />}
    </div>
  );
};

export default JobPrivacySettings;
