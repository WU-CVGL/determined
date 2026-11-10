import { JobCapabilities } from 'types';
import { DetError } from 'utils/error';

import {
  configHidden,
  isConfigNotShared,
  isLogsNotShared,
  jobDisplayName,
  logsHidden,
  shortJobId,
  titleHidden,
} from './jobPrivacy';

const hidden: JobCapabilities = {
  manageSharing: false,
  viewConfig: false,
  viewLogs: false,
  viewTitle: false,
};
const shared: JobCapabilities = {
  manageSharing: false,
  viewConfig: true,
  viewLogs: true,
  viewTitle: true,
};

describe('jobPrivacy', () => {
  it('hides only what the capabilities deny', () => {
    expect(titleHidden(hidden)).toBe(true);
    expect(logsHidden(hidden)).toBe(true);
    expect(configHidden(hidden)).toBe(true);
    expect(titleHidden(shared)).toBe(false);
    expect(logsHidden({ ...shared, viewLogs: false })).toBe(true);
    // Without capabilities, e.g. from an older master, everything shows and the master decides.
    expect(titleHidden(undefined)).toBe(false);
    expect(logsHidden(undefined)).toBe(false);
    expect(configHidden(undefined)).toBe(false);
  });

  it('shortens task IDs to their first group and keeps experiment numbers', () => {
    expect(shortJobId('1a2b3c4d-0000-4000-8000-000000000000')).toBe('1a2b3c4d');
    expect(shortJobId(12)).toBe('12');
  });

  it('names a job by its kind and short ID when its title is hidden', () => {
    const taskId = '1a2b3c4d-0000-4000-8000-000000000000';
    expect(jobDisplayName('Experiment', 12, undefined, hidden)).toBe('Experiment 12');
    expect(jobDisplayName('Shell', taskId, undefined, hidden)).toBe('Shell 1a2b3c4d');
    expect(jobDisplayName('Command', taskId, '', hidden)).toBe('Command 1a2b3c4d');
    expect(jobDisplayName('Generic Task', taskId, undefined)).toBe('Generic Task 1a2b3c4d');
  });

  it('names a job by its title when it is shared', () => {
    expect(jobDisplayName('Experiment', 12, 'mnist', shared)).toBe('mnist');
    expect(jobDisplayName('Experiment', 12, 'mnist')).toBe('mnist');
    expect(jobDisplayName('Command', 'c', '', shared)).toBe('');
  });

  it("recognizes the master's refusals in each shape they arrive in", () => {
    // A log stream that the master refuses before its first line.
    expect(isLogsNotShared(new Error('logs not shared'))).toBe(true);
    // An error line of a stream.
    expect(isLogsNotShared({ grpcCode: 7, httpCode: 403, message: 'logs not shared' })).toBe(true);
    // A refusal carried as an error object.
    const refused = new DetError(new Response(null, { status: 403 }), {
      publicMessage: { code: 7, message: 'config not shared', reason: 'PermissionDenied' },
      publicSubject: 'Request getGenericTaskConfig failed.',
    } as unknown as ConstructorParameters<typeof DetError>[1]);
    expect(isConfigNotShared(refused)).toBe(true);
    expect(isLogsNotShared(refused)).toBe(false);
    // As the master's error handler words a refusal of a request.
    const unary = new DetError(new Response(null, { status: 403 }), {
      publicMessage: 'config not shared',
      publicSubject: 'Request getGenericTaskConfig failed.',
    });
    expect(isConfigNotShared(unary)).toBe(true);
    expect(isConfigNotShared('config not shared')).toBe(true);
    expect(isLogsNotShared(new Error('not found'))).toBe(false);
    expect(isLogsNotShared(undefined)).toBe(false);
  });
});
