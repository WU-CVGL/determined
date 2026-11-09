import { Table } from 'antd';
import Alert from 'hew/Alert';
import Button from 'hew/Button';
import Row from 'hew/Row';
import Select, { Option } from 'hew/Select';
import React, { useCallback, useEffect, useMemo, useState } from 'react';

import Section from 'components/Section';
import { getJobPrivacyPolicy, putJobPrivacyPolicy } from 'services/api';
import { V1JobContentKind, V1JobPrivacyRule } from 'services/api-ts-sdk';
import handleError from 'utils/error';
import { JOB_CONTENT_KINDS, JobContentKind, masterErrorText, policyRule } from 'utils/jobPrivacy';

import css from './JobPrivacy.module.scss';

const ALLOWED = 'allowed';
const FORBIDDEN = 'forbidden';
const EVERYONE = 'everyone';
const OWNER_ONLY = 'owner-only';

const ROW_LABELS: Record<JobContentKind, string> = {
  [V1JobContentKind.TITLE]: 'Titles',
  [V1JobContentKind.LOGS]: 'Logs',
  [V1JobContentKind.CONFIG]: 'Config',
};

interface PolicyRow {
  kind: JobContentKind;
  newJobsEveryone: boolean;
  sharingAllowed: boolean;
}

const rowsOf = (rules: V1JobPrivacyRule[]): PolicyRow[] =>
  JOB_CONTENT_KINDS.map((kind) => {
    const rule = policyRule(rules, kind);
    return {
      kind,
      newJobsEveryone: rule.newJobAudience?.allUsers === true,
      sharingAllowed: rule.sharingAllowed === true,
    };
  });

/**
 * The Admin "Job Privacy" tab: whether sharing of each kind of job content is allowed, and who
 * new jobs share it with. Save sends the whole policy, one rule per kind.
 */
const JobPrivacy: React.FC = () => {
  const [saved, setSaved] = useState<PolicyRow[]>();
  const [rows, setRows] = useState<PolicyRow[]>();
  const [loadError, setLoadError] = useState<string>();
  const [saveError, setSaveError] = useState<string>();
  const [isSaving, setIsSaving] = useState(false);

  useEffect(() => {
    const canceler = new AbortController();
    getJobPrivacyPolicy({}, { signal: canceler.signal })
      .then((rules) => {
        setSaved(rowsOf(rules));
        setRows(rowsOf(rules));
      })
      .catch((e) => {
        if (canceler.signal.aborted) return;
        setLoadError(masterErrorText(e));
        handleError(e, { silent: true });
      });
    return () => canceler.abort();
  }, []);

  const updateRow = useCallback((kind: JobContentKind, change: Partial<PolicyRow>) => {
    setRows((prev) => prev?.map((row) => (row.kind === kind ? { ...row, ...change } : row)));
  }, []);

  const handleSave = useCallback(async () => {
    if (!rows) return;
    setIsSaving(true);
    setSaveError(undefined);
    try {
      const rules = await putJobPrivacyPolicy({
        rules: rows.map((row) => ({
          kind: row.kind,
          newJobAudience: { allUsers: row.newJobsEveryone },
          sharingAllowed: row.sharingAllowed,
        })),
      });
      setSaved(rowsOf(rules));
      setRows(rowsOf(rules));
    } catch (e) {
      setSaveError(masterErrorText(e));
      handleError(e, { silent: true });
    } finally {
      setIsSaving(false);
    }
  }, [rows]);

  const columns = useMemo(
    () => [
      {
        key: 'content',
        render: (_: unknown, row: PolicyRow) => ROW_LABELS[row.kind],
        title: 'Content',
      },
      {
        key: 'sharing',
        render: (_: unknown, row: PolicyRow) => (
          <div data-testid={`job-privacy-sharing-${row.kind}`}>
            <Select
              disabled={isSaving}
              searchable={false}
              value={row.sharingAllowed ? ALLOWED : FORBIDDEN}
              width={140}
              onChange={(value) => updateRow(row.kind, { sharingAllowed: value === ALLOWED })}>
              <Option value={ALLOWED}>Allowed</Option>
              <Option value={FORBIDDEN}>Forbidden</Option>
            </Select>
          </div>
        ),
        title: 'Sharing',
      },
      {
        key: 'newJobs',
        render: (_: unknown, row: PolicyRow) => (
          <div data-testid={`job-privacy-new-jobs-${row.kind}`}>
            <Select
              disabled={isSaving}
              searchable={false}
              value={row.newJobsEveryone ? EVERYONE : OWNER_ONLY}
              width={140}
              onChange={(value) => updateRow(row.kind, { newJobsEveryone: value === EVERYONE })}>
              <Option value={EVERYONE}>Everyone</Option>
              <Option value={OWNER_ONLY}>Owner only</Option>
            </Select>
          </div>
        ),
        title: 'New jobs',
      },
    ],
    [isSaving, updateRow],
  );

  const changed = !!rows && JSON.stringify(rows) !== JSON.stringify(saved);

  return (
    <Section className={css.base}>
      {loadError && <Alert message={loadError} type="error" />}
      <Table<PolicyRow>
        columns={columns}
        dataSource={rows}
        loading={!rows && !loadError}
        pagination={false}
        rowKey="kind"
        size="small"
      />
      {saveError && <Alert message={saveError} type="error" />}
      <Row>
        <Button
          disabled={!changed || isSaving}
          loading={isSaving}
          type="primary"
          onClick={handleSave}>
          Save
        </Button>
      </Row>
    </Section>
  );
};

export default JobPrivacy;
