import { Popover } from 'antd';
import dayjs from 'dayjs';
import Icon from 'hew/Icon';
import { useTheme } from 'hew/Theme';
import React, {
  CSSProperties,
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from 'react';

import Link from 'components/Link';
import { slotStateToLabel } from 'constants/states';
import { paths } from 'routes/utils';
import { V1GpuInfo, V1GpuP2p, V1GpuTopology } from 'services/api-ts-sdk';
import { Agent, Resource, SlotState } from 'types';
import { DEFAULT_DATETIME_FORMAT } from 'utils/datetime';
import {
  agentOffLabel,
  firstNotOkStatus,
  GPU_EXCLUDED_TEXT,
  GPU_HEALTH_LABELS,
  GPU_NARROW_LINK_TEXT,
  GPU_P2P_STATUS_CODES,
  GPU_P2P_STATUS_LEGEND,
  gpuDisplayOrder,
  gpuHealthSummary,
  GpuHealthWord,
  gpuHealthWord,
  gpuLabel,
  gpuLinkLookup,
  gpuSlotCountText,
  gpuTopologySummary,
  linkLevelName,
  linkText,
  numaGroups,
  nvmlErrorsText,
  OffLabel,
  pairLevels,
  recentXidTexts,
  shortPciBusId,
  slotFillColor,
  slotFillEdgeColor,
  slotFillOnColor,
  slotFillState,
  slotOffLabel,
  switchGroups,
} from 'utils/gpuTopology';

import css from './GpuTopology.module.scss';

/** The agent docs section that explains the GPU topology and health. */
export const GPU_TOPOLOGY_DOCS_PATH = paths.docs(
  '/reference/deploy/agent-config-reference.html#agent-gpu-topology',
);

interface Props {
  agent: Agent;
}

interface GpuProps {
  agentId: string;
  /** Why the whole agent takes no new work, if it does not. */
  agentOff?: OffLabel;
  gpu: V1GpuInfo;
  resource?: Resource;
  topo: V1GpuTopology;
}

/** The colours of a tile or legend swatch with the given fill. */
const fillStyle = (fill: SlotState): CSSProperties => {
  const edge = slotFillEdgeColor(fill);
  return {
    '--gpu-tile-fill': slotFillColor(fill),
    '--gpu-tile-on': slotFillOnColor(fill),
    ...(edge ? { '--gpu-tile-edge': edge } : {}),
  } as CSSProperties;
};

const gpuName = (gpu: V1GpuInfo): string =>
  gpu.excluded ? `Excluded GPU ${gpuLabel(gpu)}` : `Slot ${gpu.deviceId}`;

/** The label of a striped tile: a slot that takes no new work, or an excluded GPU. */
const offLabel = (gpu: V1GpuInfo, resource?: Resource, agentOff?: OffLabel): string | undefined =>
  gpu.excluded ? 'excluded' : slotOffLabel(resource, agentOff);

/**
 * The fill label of a slot. A slot without a slot record is unknown, as the count line and
 * `det agent describe` say; its tile keeps the Free colour, which is the neutral one.
 */
const fillLabel = (resource?: Resource): string =>
  resource ? slotStateToLabel[slotFillState(resource)] : 'Unknown';

const stateText = (gpu: V1GpuInfo, resource?: Resource, agentOff?: OffLabel): string => {
  if (gpu.excluded) return 'Excluded';
  let text = resource ? fillLabel(resource) : 'Unknown (no slot record)';
  const containerState = resource?.container?.state;
  if (slotFillState(resource) === SlotState.Pending && containerState) {
    text += ` (${containerState.toLowerCase()})`;
  }
  const off = offLabel(gpu, resource, agentOff);
  return off ? `${text}, ${off}` : text;
};

export const HealthDot: React.FC<{ word: GpuHealthWord; decorative?: boolean }> = ({
  word,
  decorative,
}) =>
  decorative ? (
    <span aria-hidden className={`${css.dot} ${css[word]}`} />
  ) : (
    <span
      aria-label={`GPU health: ${GPU_HEALTH_LABELS[word]}`}
      className={`${css.dot} ${css[word]}`}
      role="img"
    />
  );

/**
 * The GPU's identity and the facts of its health: the PCIe link and the NVML errors, both measured
 * at agent start, the collection time (the agent's clock), and the recent critical XIDs when there
 * are any.
 */
export const GpuDetails: React.FC<GpuProps> = ({ agentId, agentOff, gpu, resource, topo }) => {
  const word = gpuHealthWord(gpu.health);
  const xids = recentXidTexts(gpu);
  const collectedAt = topo.collectedAt
    ? dayjs(topo.collectedAt).format(DEFAULT_DATETIME_FORMAT)
    : 'unknown';
  return (
    <div className={css.details}>
      <p className={css.detailsTitle}>
        <HealthDot decorative word={word} />
        {gpuName(gpu)} on {agentId}: {GPU_HEALTH_LABELS[word]}
      </p>
      <dl>
        <dt>Slot</dt>
        <dd>{gpu.excluded ? 'none' : gpu.deviceId}</dd>
        <dt>State</dt>
        <dd>{stateText(gpu, resource, agentOff)}</dd>
        <dt>UUID</dt>
        <dd>
          <code>{gpu.uuid}</code>
        </dd>
        <dt>Bus ID</dt>
        <dd>{gpu.pciBusId ? <code>{gpu.pciBusId}</code> : 'unknown'}</dd>
        <dt>NUMA</dt>
        <dd>{gpu.numaNode >= 0 ? gpu.numaNode : 'unknown'}</dd>
        <dt>PCIe link</dt>
        <dd>{linkText(gpu)}</dd>
        <dt>NVML errors</dt>
        <dd>{nvmlErrorsText(topo, gpu)}</dd>
        <dt>Collected at</dt>
        <dd>{collectedAt}</dd>
        {xids.length > 0 && (
          <>
            <dt>Recent critical XIDs</dt>
            <dd>
              {xids.map((text) => (
                <div key={text}>{text}</div>
              ))}
            </dd>
          </>
        )}
      </dl>
      {gpu.excluded && <p>{GPU_EXCLUDED_TEXT}</p>}
      {word === 'narrow' && (
        <p>
          {GPU_NARROW_LINK_TEXT}{' '}
          <Link external path={GPU_TOPOLOGY_DOCS_PATH} popout>
            GPU topology and health
          </Link>
        </p>
      )}
      {topo.unknownReason && <p>GPU topology unknown: {topo.unknownReason}</p>}
    </div>
  );
};

/** How long the pointer rests on the button before the details show, as antd's tooltips wait. */
const HOVER_DELAY_MS = 100;

/**
 * The details of a GPU in one popup below its button, for hover, focus and pin alike, so that a
 * pin keeps the popup where it is. Hover or focus shows it as a tooltip that takes no pointer
 * events, so it never covers a click on the tiles under it; leaving the button or a blur hides it.
 * A click pins it as a dialog with a close button. Escape, the close button, a click outside or a
 * second click on the button close it. The popup is portalled to the end of the page, so a pin
 * from the keyboard moves focus into it, and closing it with focus inside gives focus back to the
 * button.
 *
 * The popup takes no trigger of antd: with a click trigger, antd would toggle the open state on the
 * click that pins a popup that hover already opened, and close it.
 */
const GpuInfoButton: React.FC<GpuProps> = (props) => {
  const {
    themeSettings: { className: themeClass },
  } = useTheme();
  const [pinned, setPinned] = useState(false);
  // Shown by hover or focus, until the pointer or focus leaves the button or the popup closes.
  const [peeking, setPeeking] = useState(false);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const popupRef = useRef<HTMLDivElement>(null);
  const hoverTimer = useRef<number>();
  const pinnedFromKeyboard = useRef(false);
  const tooltipId = useId();
  const { gpu } = props;
  const what = gpu.excluded ? `excluded GPU ${gpuLabel(gpu)}` : `slot ${gpu.deviceId}`;
  const label = `Details for ${what} on ${props.agentId}`;

  const stopHoverTimer = useCallback(() => window.clearTimeout(hoverTimer.current), []);
  const peek = useCallback(() => setPeeking(true), []);
  const unpeek = useCallback(() => {
    stopHoverTimer();
    setPeeking(false);
  }, [stopHoverTimer]);
  const onMouseEnter = useCallback(() => {
    stopHoverTimer();
    hoverTimer.current = window.setTimeout(peek, HOVER_DELAY_MS);
  }, [peek, stopHoverTimer]);
  const close = useCallback(() => {
    if (popupRef.current?.contains(document.activeElement)) buttonRef.current?.focus();
    // After the focus moves: focus on the button would show the details again.
    setPinned(false);
    unpeek();
  }, [unpeek]);
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape') close();
    },
    [close],
  );
  // Enter and Space click a button with detail 0; a pointer click has detail 1 or more.
  const onClick = useCallback(
    (e: React.MouseEvent) => {
      if (pinned) {
        close();
        return;
      }
      pinnedFromKeyboard.current = e.detail === 0;
      setPinned(true);
    },
    [close, pinned],
  );
  // antd still closes a popup on a touch outside it.
  const onOpenChange = useCallback(
    (open: boolean) => {
      if (!open) close();
    },
    [close],
  );

  // No hover timer outlives the button.
  useEffect(() => stopHoverTimer, [stopHoverTimer]);

  // A pinned popup closes on a press outside it and its button.
  useEffect(() => {
    if (!pinned) return;
    const onMouseDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (!buttonRef.current?.contains(target) && !popupRef.current?.contains(target)) close();
    };
    document.addEventListener('mousedown', onMouseDown);
    return () => document.removeEventListener('mousedown', onMouseDown);
  }, [close, pinned]);

  useEffect(() => {
    if (!pinned || !pinnedFromKeyboard.current) return;
    pinnedFromKeyboard.current = false;
    // The popup may still be hidden for a frame or two while it appears.
    let frame = 0;
    let tries = 0;
    const focusDialog = () => {
      const dialog = popupRef.current;
      dialog?.focus();
      if (document.activeElement !== dialog && tries++ < 10) {
        frame = requestAnimationFrame(focusDialog);
      }
    };
    focusDialog();
    return () => cancelAnimationFrame(frame);
  }, [pinned]);

  return (
    <Popover
      content={
        <div
          aria-label={pinned ? label : undefined}
          className={css.popup}
          ref={popupRef}
          role={pinned ? 'dialog' : undefined}
          tabIndex={pinned ? -1 : undefined}
          onKeyDown={onKeyDown}>
          {pinned && (
            <button aria-label="Close details" className={css.close} type="button" onClick={close}>
              <Icon decorative name="close" size="small" />
            </button>
          )}
          <GpuDetails {...props} />
        </div>
      }
      id={tooltipId}
      open={pinned || peeking}
      overlayClassName={themeClass}
      // The popup has the padding, so a click anywhere in the box is a click in the popup.
      overlayInnerStyle={{ padding: 0 }}
      overlayStyle={pinned ? undefined : { pointerEvents: 'none' }}
      placement="bottomLeft"
      showArrow={false}
      trigger={[]}
      onOpenChange={onOpenChange}>
      <button
        aria-describedby={peeking && !pinned ? tooltipId : undefined}
        aria-expanded={pinned}
        aria-label={label}
        className={css.info}
        ref={buttonRef}
        type="button"
        onBlur={unpeek}
        onClick={onClick}
        onFocus={peek}
        onKeyDown={onKeyDown}
        onMouseEnter={onMouseEnter}
        onMouseLeave={unpeek}>
        <Icon decorative name="info" size="small" />
      </button>
    </Popover>
  );
};

const GpuTile: React.FC<GpuProps> = (props) => {
  const { agentOff, gpu, resource } = props;
  const fill = gpu.excluded ? SlotState.Free : slotFillState(resource);
  const off = offLabel(gpu, resource, agentOff);
  const word = gpuHealthWord(gpu.health);
  // The fill state, then the stripe label: a draining slot can still run work. An excluded GPU
  // has the Free fill only for its colour, so its name leaves the fill out.
  const name = gpu.excluded
    ? `${gpuName(gpu)}, excluded`
    : `${gpuName(gpu)}, ${fillLabel(resource)}${off ? `, ${off}` : ''}`;
  const classes = [css.tile];
  if (off) classes.push(css.striped);
  return (
    <div
      aria-label={name}
      className={classes.join(' ')}
      data-fill={fill}
      role="group"
      style={fillStyle(fill)}>
      {/* An excluded GPU is not a slot: it has no slot id, as in `det agent describe`. */}
      <span className={css.tileName}>{gpu.excluded ? '\u2013' : gpu.deviceId}</span>
      <GpuInfoButton {...props} />
      <HealthDot word={word} />
      <span className={css.bus}>{gpu.pciBusId ? shortPciBusId(gpu.pciBusId) : 'no bus id'}</span>
      {off ? (
        <span className={css.flag}>{off}</span>
      ) : (
        <span className={css.state}>{fillLabel(resource)}</span>
      )}
    </div>
  );
};

const PairMatrix: React.FC<{ agentId: string; topo: V1GpuTopology }> = ({ agentId, topo }) => {
  const gpus = gpuDisplayOrder(topo);
  const lookup = gpuLinkLookup(topo);
  const label = (g: V1GpuInfo) => (g.excluded ? gpuLabel(g) : String(g.deviceId));
  if (gpus.length < 2) return null;
  return (
    <details className={css.matrix}>
      <summary>Pairwise matrix</summary>
      <div className={css.scroll}>
        <table>
          <caption>Link levels and P2P between the GPUs of {agentId}</caption>
          <thead>
            <tr>
              <th scope="col">GPU</th>
              {gpus.map((g) => (
                <th key={g.uuid} scope="col">
                  {label(g)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {gpus.map((a) => (
              <tr key={a.uuid}>
                <th scope="row">{label(a)}</th>
                {gpus.map((b) => {
                  if (a.uuid === b.uuid) {
                    return (
                      <td className={css.self} key={b.uuid}>
                        X
                      </td>
                    );
                  }
                  const pair = lookup(a, b);
                  const nvlinks = pair && pair.link.nvlinks > 0 ? `+NV${pair.link.nvlinks}` : '';
                  let mark = '';
                  if (pair?.link.p2p === V1GpuP2p.NOTUSABLE) {
                    const status = firstNotOkStatus(pair);
                    mark = `no P2P: ${status ? GPU_P2P_STATUS_CODES[status] : '?'}`;
                  } else if (pair?.link.p2p !== V1GpuP2p.USABLE) {
                    mark = 'P2P ?';
                  }
                  return (
                    <td key={b.uuid}>
                      {(linkLevelName(pair?.link.level) || '?') + nvlinks}
                      {mark && <span className={css.p2pMark}>{mark}</span>}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className={css.matrixNote}>
        Link levels as NVML reports them; slots by id, excluded GPUs by bus id. A second line marks
        a pair without usable P2P with its first status other than OK, or P2P ? when P2P is unknown:{' '}
        {GPU_P2P_STATUS_LEGEND}.
      </p>
    </details>
  );
};

/** The text legend of the panel: colour is never the only signal. */
export const GpuTopologyLegend: React.FC = () => (
  <div aria-label="GPU topology legend" className={css.legend} role="note">
    <span className={css.legendGroup}>
      <b>Fill</b>
      {[SlotState.Free, SlotState.Pending, SlotState.Running].map((state) => (
        <span className={css.legendItem} key={state}>
          <span aria-hidden className={css.swatch} style={fillStyle(state)} />
          {slotStateToLabel[state]}
        </span>
      ))}
    </span>
    <span className={css.legendGroup}>
      <b>Health</b>
      {(['ok', 'narrow', 'error', 'unknown'] as GpuHealthWord[]).map((word) => (
        <span className={css.legendItem} key={word}>
          <HealthDot decorative word={word} />
          {GPU_HEALTH_LABELS[word]}
        </span>
      ))}
    </span>
    <span className={css.legendGroup}>
      <span className={css.legendItem}>
        <span aria-hidden className={`${css.swatch} ${css.striped}`} />
        striped = disabled, draining or excluded
      </span>
      <span className={css.legendItem}>
        <Icon decorative name="info" size="small" />
        hover or focus for details, click to pin
      </span>
    </span>
  </div>
);

const GpuTopology: React.FC<Props> = ({ agent }) => {
  const topo = agent.gpuTopology;
  const resources = useMemo(
    () => new Map(agent.resources.map((r) => [String(r.id), r])),
    [agent.resources],
  );
  if (!topo) return null;

  const gpus = gpuDisplayOrder(topo);
  const agentOff = agentOffLabel(agent);
  const resourceOf = (gpu: V1GpuInfo) =>
    gpu.excluded ? undefined : resources.get(String(gpu.deviceId));
  const tile = (gpu: V1GpuInfo) => (
    <GpuTile
      agentId={agent.id}
      agentOff={agentOff}
      gpu={gpu}
      key={gpu.uuid}
      resource={resourceOf(gpu)}
      topo={topo}
    />
  );

  let body: React.ReactNode;
  if (topo.unknownReason) {
    body = (
      <>
        <p className={css.unknownText}>GPU topology unknown: {topo.unknownReason}</p>
        <div className={`${css.tiles} ${css.flat}`}>{gpus.map(tile)}</div>
      </>
    );
  } else {
    body = (
      <>
        <div className={css.numaRow}>
          {numaGroups(gpus).map(({ numaNode, gpus: numaGpus }) => {
            const groups = switchGroups(topo, numaGpus);
            const switches = groups.filter((g) => g.length > 1);
            const singles = groups.filter((g) => g.length === 1).flat();
            const name = numaNode >= 0 ? `NUMA ${numaNode}` : 'NUMA unknown';
            return (
              <section aria-label={name} className={css.numa} key={numaNode}>
                <h4>{name}</h4>
                {switches.length > 0 && (
                  <div className={css.switches}>
                    {switches.map((group) => (
                      <div className={css.switch} key={group[0].uuid}>
                        <h5>PCIe switch ({pairLevels(topo, group).join('/')})</h5>
                        <div className={`${css.tiles} ${css.flat}`}>{group.map(tile)}</div>
                      </div>
                    ))}
                  </div>
                )}
                {singles.length > 0 && (
                  <div className={`${css.tiles} ${css.flat}`}>{singles.map(tile)}</div>
                )}
              </section>
            );
          })}
        </div>
        <PairMatrix agentId={agent.id} topo={topo} />
      </>
    );
  }

  return (
    <article aria-label={`GPU topology of agent ${agent.id}`} className={css.agent}>
      <div className={css.agentHead}>
        <h3>{agent.id}</h3>
        <span className={css.counts}>{gpuSlotCountText(gpus, resourceOf, agentOff)}</span>
      </div>
      <dl className={css.summary}>
        <div>
          <dt>GPU topology</dt>
          <dd>{gpuTopologySummary(topo)}</dd>
        </div>
        <div>
          <dt>GPU health</dt>
          <dd>{gpuHealthSummary(topo)}</dd>
        </div>
      </dl>
      {body}
    </article>
  );
};

export default GpuTopology;
