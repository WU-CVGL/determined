import React, { useMemo } from 'react';

import Section from 'components/Section';
import UPlotChart, { Options } from 'components/UPlot/UPlotChart';
import { glasbeyColor } from 'utils/color';
import { humanReadableBytes } from 'utils/string';
import {
  alignResourceSeries,
  resourceLegend,
  ResourceMetric,
  ResourceRange,
  ResourceSeries,
} from 'utils/taskResources';

import css from './TaskResourceChart.module.scss';

interface Props {
  metric: ResourceMetric;
  range: ResourceRange;
  series: ResourceSeries[];
}

const TaskResourceChart: React.FC<Props> = ({ metric, range, series }) => {
  const aligned = useMemo(() => alignResourceSeries(series, range), [series, range]);
  const hasData = series.some((item) =>
    item.samples.some(([, value]) => value != null && Number.isFinite(value)),
  );
  const options = useMemo<Options>(() => {
    const format = (value: number): string =>
      metric.unit === 'bytes'
        ? humanReadableBytes(value)
        : `${Number(value.toFixed(2))} ${metric.unit}`;
    const legend = resourceLegend(series);
    return {
      axes: [{}, { size: 85, values: (_plot, values) => values.map(format) }],
      cursor: { drag: { x: true, y: false } },
      height: 260,
      legend: { live: true, show: true },
      // uPlot builds the legend itself; each entry shows the series' details on hover. A plugin
      // keeps the chart sync's own hooks.
      plugins: [
        {
          hooks: {
            init: (plot) => {
              const rows = plot.root.querySelectorAll<HTMLElement>('.u-legend .u-series');
              // The live legend has a leading row for the time.
              const offset = rows.length - legend.length;
              legend.forEach(({ details }, index) => {
                const row = rows[index + offset];
                if (row && details) row.title = details;
              });
            },
          },
        },
      ],
      scales: { x: { max: range.end, min: range.start, time: true } },
      series: [
        { label: 'Time', value: (_plot, value) => new Date(value * 1000).toLocaleString() },
        ...legend.map(({ label }, index) => ({
          label,
          points: { show: false },
          spanGaps: false,
          stroke: glasbeyColor(index),
          value: (_plot: unknown, value: number | null) => (value == null ? 'N/A' : format(value)),
          width: 2,
        })),
      ],
    };
  }, [metric, range, series]);

  return (
    <Section bodyBorder title={metric.title}>
      {hasData ? (
        <div className={css.chart}>
          <UPlotChart data={aligned} options={options} />
        </div>
      ) : (
        <p>No attributed samples in this time range.</p>
      )}
    </Section>
  );
};

export default TaskResourceChart;
