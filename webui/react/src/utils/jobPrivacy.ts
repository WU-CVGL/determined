import { V1JobAudience, V1JobContentKind, V1JobPrivacyRule } from 'services/api-ts-sdk';
import { JobCapabilities } from 'types';

import { isObject } from './data';
import { DetError } from './error';

/*
 * What a viewer sees of a job whose title, logs or config are not shared with them. The master
 * omits that content and says so in the job's capabilities; unknown capabilities show everything,
 * and the master decides.
 */

export const LOGS_NOT_SHARED = 'Logs not shared';
export const CONFIG_NOT_SHARED = 'Config not shared';

export const titleHidden = (capabilities?: JobCapabilities): boolean =>
  capabilities?.viewTitle === false;

export const logsHidden = (capabilities?: JobCapabilities): boolean =>
  capabilities?.viewLogs === false;

export const configHidden = (capabilities?: JobCapabilities): boolean =>
  capabilities?.viewConfig === false;

/** The short form of a job's ID: an experiment's number, or the first group of a task's UUID. */
export const shortJobId = (id: string | number): string => String(id).split('-')[0];

/**
 * The name of a job as lists, headers and breadcrumbs show it: its title, or its kind and short ID
 * when the title is not shared with the viewer, e.g. "Experiment 12" or "Shell 1a2b3c4d".
 */
export const jobDisplayName = (
  kindLabel: string,
  id: string | number,
  name?: string,
  capabilities?: JobCapabilities,
): string =>
  titleHidden(capabilities) || name === undefined ? `${kindLabel} ${shortJobId(id)}` : name;

/*
 * The texts of an error: a refusal reaches the WebUI as an Error with the master's message (log
 * streams), as the master's error object (stream lines), or as a DetError whose public message is
 * the master's error object (other requests).
 */
const errorMessages = (e: unknown, depth = 0): string[] => {
  if (depth > 3) return [];
  if (typeof e === 'string') return [e];
  if (e instanceof DetError) {
    return [e.message, e.publicMessage, e.sourceErr].flatMap((part) =>
      errorMessages(part, depth + 1),
    );
  }
  if (e instanceof Error) return [e.message];
  if (isObject(e)) return errorMessages((e as { message?: unknown }).message, depth + 1);
  return [];
};

const isRefusal = (e: unknown, phrase: string): boolean =>
  errorMessages(e).some((message) => message.toLowerCase().includes(phrase));

/** Whether the master refused a job's logs because they are not shared with the viewer. */
export const isLogsNotShared = (e: unknown): boolean => isRefusal(e, 'logs not shared');

/** Whether the master refused a job's config because it is not shared with the viewer. */
export const isConfigNotShared = (e: unknown): boolean => isRefusal(e, 'config not shared');

/** The kinds of job content whose sharing a policy rule or a job's sharing sets. */
export const JOB_CONTENT_KINDS = [
  V1JobContentKind.TITLE,
  V1JobContentKind.LOGS,
  V1JobContentKind.CONFIG,
] as const;
export type JobContentKind = (typeof JOB_CONTENT_KINDS)[number];

/**
 * Whether an audience is all users: true or false when it is set, undefined when it is not (a
 * job created before job privacy, which is public, or a user's new-job default left to the
 * cluster's).
 */
export const audienceAllUsers = (audience?: V1JobAudience | null): boolean | undefined =>
  audience === undefined || audience === null ? undefined : !!audience.allUsers;

/** The policy rule of a kind; a kind without a rule is forbidden, as the master treats it. */
export const policyRule = (rules: V1JobPrivacyRule[], kind: JobContentKind): V1JobPrivacyRule =>
  rules.find((rule) => rule.kind === kind) ?? {
    kind,
    newJobAudience: { allUsers: false },
    sharingAllowed: false,
  };

/** Whether the policy allows sharing a kind. */
export const sharingAllowed = (rules: V1JobPrivacyRule[], kind: JobContentKind): boolean =>
  policyRule(rules, kind).sharingAllowed === true;

/** The master's text of a failed request, as it sent it. */
export const masterErrorText = (e: unknown): string => {
  if (e instanceof DetError) {
    const text = e.publicMessage || e.message;
    if (text) return text;
  }
  if (e instanceof Error) return e.message;
  return String(e);
};
