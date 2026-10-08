import Tooltip from 'hew/Tooltip';
import React, { PropsWithChildren, useEffect, useMemo, useRef, useState } from 'react';

import Section from 'components/Section';
import { V1JobPlacement } from 'services/api-ts-sdk';
import { Agent, Resource, SlotsRecord } from 'types';

import css from './ClusterTopology.module.scss';
import GpuTopology, { GpuTopologyLegend } from './GpuTopology';

interface NodeElementProps {
  name: string;
  resources: Resource[];
  slots?: SlotsRecord;
}

interface Props {
  /** The GPUs whose tiles to highlight, by agent. */
  highlight?: V1JobPlacement[];
  nodes: Agent[];
}

const NodeElement: React.FC<PropsWithChildren<NodeElementProps>> = ({ name, slots, resources }) => {
  const [containerWidth, setContainerWidth] = useState(0);
  const shouldTruncate = useMemo(() => name.length > 5, [name]);
  const slotsContainer = useRef<HTMLSpanElement>(null);
  const slotsData = useMemo(
    () => (slots !== undefined ? Object.values(slots) : resources),
    [slots, resources],
  );
  const singleSlot = slotsData.length === 1;
  const coupleSlot = slotsData.length === 2;
  const styles = [css.nodeSlot];

  if (singleSlot) styles.push(css.singleSlot);
  if (coupleSlot) styles.push(css.coupleSlot);

  useEffect(() => {
    setContainerWidth(slotsContainer.current?.getBoundingClientRect().width || 0);
  }, []);

  return (
    <div className={css.node}>
      {shouldTruncate ? (
        <Tooltip content={name}>
          <span className={css.nodeName} style={{ maxWidth: containerWidth }}>
            {name}
          </span>
        </Tooltip>
      ) : (
        <span className={css.nodeName}>{name}</span>
      )}
      <span className={css.nodeCluster} ref={slotsContainer}>
        {slotsData.map(({ container }, idx) => (
          <span
            className={`${styles.join(' ')} ${container ? css.active : ''}`}
            key={`slot${idx}`}
          />
        ))}
      </span>
    </div>
  );
};

const Topology: React.FC<PropsWithChildren<Props>> = ({ highlight, nodes }) => {
  // Agents that report a GPU topology get the GPU panel instead of the slot strip.
  const withGpuTopology = nodes.some((node) => node.gpuTopology);
  const highlighted = useMemo(
    () => new Map((highlight ?? []).map(({ agentId, deviceIds }) => [agentId, new Set(deviceIds)])),
    [highlight],
  );
  return (
    <Section title="Topology">
      <div className={`${css.mainContainer} ${css.nodesContainer}`}>
        {nodes.map((node) => {
          const { id, resources, slots } = node;
          if (node.gpuTopology) {
            return <GpuTopology agent={node} highlighted={highlighted.get(id)} key={id} />;
          }
          return <NodeElement key={id} name={id} resources={resources} slots={slots} />;
        })}
        {withGpuTopology && <GpuTopologyLegend />}
      </div>
    </Section>
  );
};

export default Topology;
