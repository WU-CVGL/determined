import fixture from 'fixtures/jobsSortOrder.json';
import { FullJob, Job, JobState, JobType } from 'types';

import { sortJobs } from './JobQueue.sort';

/*
 * The text cases of the Jobs page's shared fixture, which the master's tests check against Postgres
 * and Go: the Active tab orders names and users as the Jobs page does. Its ties keep the queue
 * order instead of the fixture's newest start first, so the texts are compared, not the rows.
 */

interface FixtureRow {
  id: string;
  name: string;
  owner: string | null;
}

const rows = fixture.rows as FixtureRow[];
const orders = fixture.orders as Record<string, { ascend: string[]; descend: string[] }>;
const rowOf = (job: Job): FixtureRow => rows.find((row) => row.id === job.jobId) as FixtureRow;

/** A job of each row, in queue order as the fixture lists the rows. */
const jobs: FullJob[] = rows.map((row, index) => ({
  allocatedSlots: 1,
  entityId: String(100 + index),
  isPreemptible: true,
  jobId: row.id,
  name: row.name,
  priority: 42,
  requestedSlots: 1,
  resourcePool: 'default',
  submissionTime: new Date('2026-01-01T00:00:00Z'),
  summary: { jobsAhead: index, state: JobState.SCHEDULED },
  type: JobType.EXPERIMENT,
  userId: index,
  username: row.owner ?? '',
  workspaceId: 1,
}));

/** The name the page sorts a user by; none for a row without an owner. */
const ownerName = (job: Job) => rowOf(job).owner ?? undefined;

const COLUMNS = [
  { column: 'name', fixtureKey: 'name', text: (job: Job) => rowOf(job).name },
  { column: 'user', fixtureKey: 'owner', text: ownerName },
];

describe('sortJobs on the text cases of the shared fixture', () => {
  describe.each(COLUMNS)('by $column', ({ column, fixtureKey, text }) => {
    it.each(['ascend', 'descend'] as const)(
      'orders the texts %sing as the Jobs page does, a missing one last',
      (direction) => {
        const sorted = sortJobs(jobs, column, direction === 'descend', ownerName);
        const expected = orders[fixtureKey][direction].map((id) =>
          text(jobs.find((job) => job.jobId === id) as Job),
        );
        expect(sorted.map(text)).toEqual(expected);
      },
    );

    it.each([false, true])('keeps the queue order of equal texts (descending: %s)', (desc) => {
      expect.hasAssertions();
      const sorted = sortJobs(jobs, column, desc, ownerName);
      sorted.slice(1).forEach((job, i) => {
        if (text(job) !== text(sorted[i])) return;
        expect(job.summary.jobsAhead).toBeGreaterThan(sorted[i].summary.jobsAhead);
      });
    });
  });
});
