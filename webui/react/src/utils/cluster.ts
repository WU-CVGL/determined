import { Agent, ResourcePool, ResourceState, ResourceType } from 'types';

/** Whether the user may start new work in pool. A master that does not say is taken as yes. */
export const isUsablePool = (pool: Pick<ResourcePool, 'usable'>): boolean => pool.usable !== false;

/** The pools the user may use, then the restricted ones, each in the order the master lists them. */
export const usablePoolsFirst = <T extends Pick<ResourcePool, 'usable'>>(pools: T[]): T[] => [
  ...pools.filter(isUsablePool),
  ...pools.filter((pool) => !isUsablePool(pool)),
];

export const getSlotContainerStates = (
  agents: Agent[],
  resourceType: ResourceType,
  resourcePoolName?: string,
): ResourceState[] => {
  const slotContainerStates = agents
    .filter((agent) => {
      return resourcePoolName === undefined || agent.resourcePools.includes(resourcePoolName);
    })
    .flatMap((agent) => {
      const arr: ResourceState[] = Object.entries(agent.slotStats.typeStats ?? {})
        .filter(([type]) => {
          return (
            resourceType === ResourceType.ALL ||
            resourceType === ResourceType.UNSPECIFIED ||
            type === `TYPE_${resourceType}`
          );
        })
        .flatMap(([, val]) => {
          const tempArr = Object.entries(val.states ?? {}).flatMap(([state, count]) => {
            return Array(count).fill(state.replace('STATE_', ''));
          });
          return tempArr;
        });
      return arr;
    });
  return slotContainerStates;
};
