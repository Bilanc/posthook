"use client";

import ReactEChartsCore from "echarts-for-react/lib/core";
import * as echarts from "echarts/core";
import { SankeyChart } from "echarts/charts";
import { TooltipComponent, TitleComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";

echarts.use([SankeyChart, TooltipComponent, TitleComponent, CanvasRenderer]);

interface Props {
  generated: number;
  committed: number;
}

const numberFmt = new Intl.NumberFormat("en-US");

function fmtNum(n: number): string {
  return numberFmt.format(Math.round(n));
}

function fmtPct(n: number): string {
  return `${n.toFixed(1)}%`;
}

export function SankeyFunnel({ generated, committed }: Props) {
  const lostInEdits = Math.max(0, generated - committed);
  const keptPct = generated > 0 ? (committed / generated) * 100 : 0;

  const nodes = [
    {
      name: "AI generated",
      value: generated,
      itemStyle: { color: "#6366f1" },
    },
    {
      name: "AI committed",
      value: committed,
      itemStyle: { color: "#14b8a6" },
    },
    {
      name: "Lost in edits",
      value: lostInEdits,
      itemStyle: { color: "#3f3f46" },
    },
  ];

  const links = [
    {
      source: "AI generated",
      target: "AI committed",
      value: Math.max(committed, 0.0001),
      lineStyle: { color: "#14b8a6", opacity: 0.45 },
      label: {
        show: generated > 0,
        formatter: `${fmtPct(keptPct)} kept`,
        color: "#14b8a6",
        fontWeight: 600,
        fontFamily: "ui-sans-serif, system-ui",
      },
    },
    {
      source: "AI generated",
      target: "Lost in edits",
      value: Math.max(lostInEdits, 0.0001),
      lineStyle: { color: "#3f3f46", opacity: 0.35 },
    },
  ];

  const option = {
    backgroundColor: "transparent",
    tooltip: {
      trigger: "item",
      triggerOn: "mousemove",
      backgroundColor: "#0a0a0b",
      borderColor: "#1f1f23",
      textStyle: { color: "#e8e8ea", fontSize: 12 },
      formatter: (params: {
        dataType: "node" | "edge";
        name: string;
        value: number;
        data: { source?: string; target?: string };
      }) => {
        if (params.dataType === "edge") {
          const pct =
            generated > 0 ? ((params.value / generated) * 100).toFixed(1) : "0.0";
          return `${params.data.source} → ${params.data.target}<br/><b>${fmtNum(
            params.value,
          )}</b> lines · ${pct}% of generated`;
        }
        const pct =
          generated > 0 && params.name !== "AI generated"
            ? ` · ${((params.value / generated) * 100).toFixed(1)}% of generated`
            : "";
        return `${params.name}<br/><b>${fmtNum(params.value)}</b> lines${pct}`;
      },
    },
    series: [
      {
        type: "sankey",
        nodeAlign: "left",
        emphasis: { focus: "adjacency" },
        lineStyle: { color: "gradient", curveness: 0.5 },
        label: {
          color: "#e8e8ea",
          fontFamily: "ui-sans-serif, system-ui",
          fontSize: 12,
          formatter: (params: { name: string; value: number }) =>
            `{name|${params.name}}\n{val|${fmtNum(params.value)}}`,
          rich: {
            name: { color: "#e8e8ea", fontSize: 12, lineHeight: 16 },
            val: {
              color: "#8e8e93",
              fontSize: 11,
              fontFamily: "ui-monospace, SFMono-Regular, monospace",
              lineHeight: 14,
            },
          },
        },
        data: nodes,
        links,
      },
    ],
  };

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="flex items-baseline justify-between mb-3 gap-4">
        <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
          Funnel — AI generated → committed
        </div>
        <div className="text-xs text-[var(--color-fg-muted)] tabular-nums">
          <span className="text-[var(--color-fg)] font-medium">
            {fmtNum(generated)}
          </span>{" "}
          generated →{" "}
          <span className="text-[var(--color-fg)] font-medium">
            {fmtNum(committed)}
          </span>{" "}
          committed ·{" "}
          <span className="text-[#14b8a6] font-semibold">{fmtPct(keptPct)}</span>{" "}
          kept
        </div>
      </div>
      <ReactEChartsCore
        echarts={echarts}
        option={option}
        style={{ height: 320, width: "100%" }}
        notMerge
      />
    </div>
  );
}
