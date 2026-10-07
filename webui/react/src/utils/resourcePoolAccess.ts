import {
  DetailedUser,
  ResourcePoolAccess,
  ResourcePoolAccessChange,
  ResourcePoolAccessMode,
} from 'types';
import { DetError } from 'utils/error';
import { isApiResponse } from 'utils/service';

/*
 * Helpers of the Admin "Pool Access" tab. They decide nothing about access: the master checks every
 * change, and these only prepare the requests and describe what the master answered.
 */

/** The master refuses request bodies over 64 KiB; requests stay below with room to spare. */
export const RESOURCE_POOL_ACCESS_MAX_BODY_BYTES = 64 * 1024;
export const RESOURCE_POOL_ACCESS_BODY_BUDGET = 60 * 1024;

export type PoolAccessUsersAction = 'grant' | 'revoke';
export type PoolAccessAction = PoolAccessUsersAction | 'restrict' | 'public';

/** A user as the picker knows them: from the user list, or a group member. */
export interface PoolAccessCandidate {
  active?: boolean;
  admin?: boolean;
  username: string;
}

const quote = (s: string): string => JSON.stringify(s);

/**
 * The warnings the master gives when it answers a change of the pool, worded like the master's
 * (resourcePoolAccessWarnings): a name that is not a pool, and, while the pool is restricted, each
 * default it is. The list response has no warnings, so the table derives them here; the results of
 * a change show the master's own. With mode, the warnings are those of the pool in that mode.
 */
export const resourcePoolAccessWarnings = (
  pool: ResourcePoolAccess,
  mode: ResourcePoolAccess['mode'] = pool.mode,
): string[] => {
  const name = quote(pool.poolName);
  const warnings: string[] = [];
  if (!pool.exists) {
    warnings.push(
      `no resource pool named ${name} exists; the setting applies to a pool created with this name`,
    );
  }
  if (mode !== ResourcePoolAccessMode.Restricted) return warnings;
  const refused = `that omit resources.resource_pool are refused for users without a grant on ${name}`;
  if (pool.defaultCompute) {
    warnings.push(`${name} is the cluster's default compute pool: submissions ${refused}`);
  }
  if (pool.defaultAux) {
    warnings.push(`${name} is the cluster's default aux pool: submissions ${refused}`);
  }
  pool.workspaceDefaults.forEach((workspaceDefault) => {
    warnings.push(
      `${name} is the default ${workspaceDefault.kind} pool of workspace ${quote(
        workspaceDefault.workspace,
      )}: submissions there ${refused}`,
    );
  });
  return warnings;
};

/** Whether a pool matches the search text by its name, a granted username, or a workspace. */
export const matchesPoolSearch = (pool: ResourcePoolAccess, search: string): boolean => {
  const needle = search.trim().toLocaleLowerCase();
  if (!needle) return true;
  return [
    pool.poolName,
    ...pool.users.map((user) => user.username),
    ...pool.workspaceDefaults.map((workspaceDefault) => workspaceDefault.workspace),
  ].some((text) => text.toLocaleLowerCase().includes(needle));
};

/**
 * Splits pasted text into usernames, in order and without duplicates. A line that is a known
 * username is taken whole, since usernames may contain spaces and commas; other lines are split
 * at commas, semicolons, and white space.
 */
export const parsePastedUsernames = (text: string, known: ReadonlySet<string>): string[] => {
  const names: string[] = [];
  text.split(/\r?\n/).forEach((line) => {
    const trimmed = line.trim();
    if (!trimmed) return;
    if (known.has(trimmed)) {
      names.push(trimmed);
      return;
    }
    trimmed
      .split(/[\s,;]+/)
      .filter((name) => name.length > 0)
      .forEach((name) => names.push(name));
  });
  return Array.from(new Set(names));
};

export interface ResolvedUsernames {
  /** Users that are administrators: they may use every pool, so a grant changes nothing. */
  admins: string[];
  counts: {
    /** Usernames from the expanded groups, before removing duplicates. */
    fromGroups: number;
    fromPaste: number;
    fromUsers: number;
  };
  inactive: string[];
  /** Pasted names that match no user. The master refuses a request naming any of them. */
  unknown: string[];
  /** Every username to send, sorted and without duplicates. */
  usernames: string[];
}

/**
 * Combines the picked users, the members of the picked groups, and the pasted usernames into the
 * usernames to send. A group counts as its members at the time it was expanded.
 */
export const resolveUsernames = (
  users: readonly DetailedUser[],
  pickedUserIds: readonly number[],
  groupMembers: readonly (readonly PoolAccessCandidate[])[],
  pastedText: string,
): ResolvedUsernames => {
  const byUsername = new Map<string, PoolAccessCandidate>();
  users.forEach((user) =>
    byUsername.set(user.username, {
      active: user.isActive,
      admin: user.isAdmin,
      username: user.username,
    }),
  );
  const pickedIds = new Set(pickedUserIds);
  const fromUsers = users.filter((user) => pickedIds.has(user.id)).map((user) => user.username);
  const fromGroups = groupMembers.flat().map((member) => {
    if (!byUsername.has(member.username)) byUsername.set(member.username, member);
    return member.username;
  });
  const pasted = parsePastedUsernames(pastedText, new Set(byUsername.keys()));
  const unknown = pasted.filter((name) => !byUsername.has(name));
  const known = pasted.filter((name) => byUsername.has(name));

  const usernames = Array.from(new Set([...fromUsers, ...fromGroups, ...known])).sort((a, b) =>
    a.localeCompare(b),
  );
  return {
    admins: usernames.filter((name) => byUsername.get(name)?.admin),
    counts: {
      fromGroups: fromGroups.length,
      fromPaste: pasted.length,
      fromUsers: fromUsers.length,
    },
    inactive: usernames.filter((name) => byUsername.get(name)?.active === false),
    unknown,
    usernames,
  };
};

const encoder = new TextEncoder();
const byteLength = (s: string): number => encoder.encode(s).length;

/** The size of the request body {"usernames": [...]} that the client sends. */
export const usernamesBodyBytes = (usernames: readonly string[]): number =>
  byteLength(JSON.stringify({ usernames }));

/**
 * Splits usernames into the bodies of consecutive requests, each at most budget bytes long as
 * JSON, keeping their order. A username too long for any request goes alone, and the master
 * refuses it.
 */
export const chunkUsernames = (
  usernames: readonly string[],
  budget: number = RESOURCE_POOL_ACCESS_BODY_BUDGET,
): string[][] => {
  const emptyBytes = usernamesBodyBytes([]);
  const chunks: string[][] = [];
  let chunk: string[] = [];
  let bytes = emptyBytes;
  usernames.forEach((username) => {
    const nameBytes = byteLength(JSON.stringify(username));
    // A username after the first one is preceded by a comma.
    if (chunk.length > 0 && bytes + 1 + nameBytes > budget) {
      chunks.push(chunk);
      chunk = [];
      bytes = emptyBytes;
    }
    bytes += nameBytes + (chunk.length > 0 ? 1 : 0);
    chunk.push(username);
  });
  if (chunk.length > 0) chunks.push(chunk);
  return chunks;
};

/** The message of a failed request, with the HTTP status when the master answered. */
export const poolAccessErrorMessage = (e: unknown): string => {
  if (e instanceof DetError) {
    const status = isApiResponse(e.sourceErr) ? `${e.sourceErr.status} ` : '';
    return `${status}${e.publicMessage || e.message}`.trim();
  }
  if (e instanceof Error) return e.message;
  return String(e);
};

/**
 * What the master answered for one pool. A request is confirmed when the master answered that it
 * succeeded. A failed request may still have been applied: the master answers a write with the
 * pool's access, read after the write, and that read can fail, or the answer can be lost.
 */
export interface PoolAccessResult {
  confirmedRequests: number;
  /** Usernames in the confirmed requests. */
  confirmedUsernames: number;
  error?: string;
  ok: boolean;
  poolName: string;
  requests: number;
  totalUsernames: number;
  /** The master's warnings from its last answer for the pool. */
  warnings: string[];
}

type UsersRequest = (params: {
  poolName: string;
  usernames: string[];
}) => Promise<ResourcePoolAccessChange>;

/**
 * Sends the usernames to each pool in turn, in requests under the body limit. A failed request
 * ends that pool, whose earlier requests stay applied, and the other pools go on. Nothing is
 * retried.
 */
export const changeUsersInPools = async (
  poolNames: readonly string[],
  usernames: readonly string[],
  request: UsersRequest,
  budget: number = RESOURCE_POOL_ACCESS_BODY_BUDGET,
): Promise<PoolAccessResult[]> => {
  const chunks = chunkUsernames(usernames, budget);
  const results: PoolAccessResult[] = [];
  for (const poolName of poolNames) {
    const result: PoolAccessResult = {
      confirmedRequests: 0,
      confirmedUsernames: 0,
      ok: true,
      poolName,
      requests: chunks.length,
      totalUsernames: usernames.length,
      warnings: [],
    };
    for (const chunk of chunks) {
      try {
        const change = await request({ poolName, usernames: chunk });
        result.confirmedRequests += 1;
        result.confirmedUsernames += chunk.length;
        result.warnings = change.warnings;
      } catch (e) {
        result.ok = false;
        result.error = poolAccessErrorMessage(e);
        break;
      }
    }
    results.push(result);
  }
  return results;
};

type ModeRequest = (params: {
  mode: ResourcePoolAccess['mode'];
  poolName: string;
}) => Promise<ResourcePoolAccessChange>;

/** Restricts each pool or makes it public, in turn; a failure does not stop the other pools. */
export const setModeInPools = async (
  poolNames: readonly string[],
  mode: ResourcePoolAccess['mode'],
  request: ModeRequest,
): Promise<PoolAccessResult[]> => {
  const results: PoolAccessResult[] = [];
  for (const poolName of poolNames) {
    try {
      const change = await request({ mode, poolName });
      results.push({
        confirmedRequests: 1,
        confirmedUsernames: 0,
        ok: true,
        poolName,
        requests: 1,
        totalUsernames: 0,
        warnings: change.warnings,
      });
    } catch (e) {
      results.push({
        confirmedRequests: 0,
        confirmedUsernames: 0,
        error: poolAccessErrorMessage(e),
        ok: false,
        poolName,
        requests: 1,
        totalUsernames: 0,
        warnings: [],
      });
    }
  }
  return results;
};
