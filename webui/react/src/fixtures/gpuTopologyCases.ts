import { readFileSync } from 'fs';
import { resolve } from 'path';

import { V1GpuTopology } from 'services/api-ts-sdk';

/**
 * Agent GPU topologies as the master's API returns them, with the `det agent list` strings they
 * give. Shared with harness/tests/cli/test_agent.py, so the CLI and the WebUI show the same
 * strings. For tests only: it reads the file from the repository.
 */
export interface GpuTopologyCase {
  name: string;
  topology: string;
  health: string;
  gpuTopology: V1GpuTopology | null;
}

export const GPU_TOPOLOGY_CASES: GpuTopologyCase[] = JSON.parse(
  readFileSync(
    resolve(__dirname, '../../../../harness/tests/fixtures/gpu_topology_cases.json'),
    'utf-8',
  ),
).cases;

/** A copy of the topology of the one case whose name starts with the prefix. */
export const gpuTopologyCase = (prefix: string): V1GpuTopology => {
  const matches = GPU_TOPOLOGY_CASES.filter((c) => c.name.startsWith(prefix));
  if (matches.length !== 1 || !matches[0].gpuTopology) throw new Error(`no case ${prefix}`);
  return structuredClone(matches[0].gpuTopology);
};
