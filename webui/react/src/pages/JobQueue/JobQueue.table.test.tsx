import { render, screen } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';
import { MemoryRouter } from 'react-router-dom';

import { FullJob, JobState, JobType } from 'types';

import { columns } from './JobQueue.table';

const hidden = { manageSharing: false, viewConfig: false, viewLogs: false, viewTitle: false };

const job = (type: JobType, entityId: string): FullJob => ({
  allocatedSlots: 1,
  capabilities: hidden,
  entityId,
  isPreemptible: true,
  jobId: `job-${entityId}`,
  // As the master lists a job whose title is not shared with the viewer.
  name: '',
  priority: 42,
  requestedSlots: 1,
  resourcePool: 'default',
  submissionTime: new Date('2026-01-01T00:00:00Z'),
  summary: { jobsAhead: 0, state: JobState.SCHEDULED },
  type,
  userId: 1,
  username: 'alice',
  workspaceId: 1,
});

const renderName = (record: FullJob) => {
  const column = columns(false).find((col) => col.key === 'name');
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <MemoryRouter>{column?.render?.(undefined, record, 0) as React.ReactNode}</MemoryRouter>
    </UIProvider>,
  );
};

describe('Job Name column', () => {
  it.each([
    [JobType.EXPERIMENT, '12', 'Experiment 12'],
    [JobType.GENERIC, '9f8e7d6c-0000-4000-8000-000000000000', 'Generic Task 9f8e7d6c'],
    [JobType.SHELL, '1a2b3c4d-0000-4000-8000-000000000000', 'Shell 1a2b3c4d'],
    [JobType.COMMAND, '1a2b3c4d-0000-4000-8000-000000000000', 'Command 1a2b3c4d'],
  ])('names a %s job whose title is not shared by its kind and short ID', (type, id, name) => {
    renderName(job(type, id));
    expect(screen.getByText(name)).toBeInTheDocument();
  });
});
