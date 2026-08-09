import { useEffect, useMemo, useState } from "react";
import { api, apiMessage } from "../../api";
import { EChart } from "../EChart";
import { useI18n } from "../../lib/i18n";
import { formatBytes } from "../../lib/utils";

interface TrafficBucket {
  periodStart: string;
  rxBytes: number;
  txBytes: number;
}

interface TrafficAnalysis {
  status?: string;
  range?: string;
  buckets?: TrafficBucket[];
}

interface DailyTraffic {
  key: string;
  label: string;
  rxBytes: number;
  txBytes: number;
}

function localDayKey(date: Date): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, "0");
  const day = String(date.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

function lastSevenDays(buckets: TrafficBucket[]): DailyTraffic[] {
  const byDay = new Map<string, TrafficBucket>();
  for (const bucket of buckets) {
    const date = new Date(bucket.periodStart);
    if (Number.isNaN(date.getTime())) continue;
    const key = localDayKey(date);
    const current = byDay.get(key);
    byDay.set(key, {
      periodStart: bucket.periodStart,
      rxBytes: (current?.rxBytes || 0) + (Number(bucket.rxBytes) || 0),
      txBytes: (current?.txBytes || 0) + (Number(bucket.txBytes) || 0),
    });
  }

  const result: DailyTraffic[] = [];
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  for (let offset = 6; offset >= 0; offset -= 1) {
    const date = new Date(today);
    date.setDate(today.getDate() - offset);
    const key = localDayKey(date);
    const value = byDay.get(key);
    result.push({
      key,
      label: date.toLocaleDateString(undefined, { month: "2-digit", day: "2-digit" }),
      rxBytes: value?.rxBytes || 0,
      txBytes: value?.txBytes || 0,
    });
  }
  return result;
}

export function OverviewTrafficChart({ deviceId }: { deviceId: string }) {
  const { t } = useI18n();
  const [buckets, setBuckets] = useState<TrafficBucket[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    const load = async (initial = false) => {
      if (initial) setLoading(true);
      try {
        const result = await api<TrafficAnalysis>(`/traffic/analysis?range=week&device_id=${encodeURIComponent(deviceId)}`);
        if (!cancelled) {
          setBuckets(result.buckets || []);
          setError("");
        }
      } catch (err) {
        if (!cancelled) setError(apiMessage(err) || t("流量分析加载失败"));
      } finally {
        if (!cancelled && initial) setLoading(false);
      }
    };
    void load(true);
    const timer = window.setInterval(() => void load(), 30_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [deviceId, t]);

  const days = useMemo(() => lastSevenDays(buckets), [buckets]);
  const totals = useMemo(() => days.reduce(
    (value, day) => ({ rx: value.rx + day.rxBytes, tx: value.tx + day.txBytes }),
    { rx: 0, tx: 0 },
  ), [days]);

  const option = useMemo(() => ({
    animationDuration: 350,
    color: ["#0ea5e9", "#8b5cf6"],
    tooltip: {
      trigger: "axis",
      formatter: (items: Array<{ marker?: string; seriesName?: string; value?: number; axisValueLabel?: string }>) => {
        const title = items[0]?.axisValueLabel || "";
        return [title, ...items.map((item) => `${item.marker || ""}${item.seriesName || ""}: ${formatBytes(Number(item.value) || 0)}`)].join("<br/>");
      },
    },
    legend: {
      top: 0,
      right: 0,
      textStyle: { color: "#64748b" },
      data: [t("下载"), t("上传")],
    },
    grid: { top: 42, right: 18, bottom: 24, left: 62 },
    xAxis: {
      type: "category",
      boundaryGap: false,
      data: days.map((day) => day.label),
      axisLine: { lineStyle: { color: "#cbd5e1" } },
      axisTick: { show: false },
      axisLabel: { color: "#64748b" },
    },
    yAxis: {
      type: "value",
      min: 0,
      axisLabel: { color: "#64748b", formatter: (value: number) => formatBytes(value) },
      splitLine: { lineStyle: { color: "rgba(148, 163, 184, 0.18)" } },
    },
    series: [
      {
        name: t("下载"),
        type: "line",
        smooth: true,
        showSymbol: true,
        symbolSize: 6,
        data: days.map((day) => day.rxBytes),
        lineStyle: { width: 3 },
        areaStyle: { opacity: 0.1 },
      },
      {
        name: t("上传"),
        type: "line",
        smooth: true,
        showSymbol: true,
        symbolSize: 6,
        data: days.map((day) => day.txBytes),
        lineStyle: { width: 3 },
        areaStyle: { opacity: 0.08 },
      },
    ],
  }), [days, t]);

  return (
    <div className="ui-panel-muted p-4 lg:p-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="text-sm font-bold text-gray-900 dark:text-white">{t("最近 7 天流量")}</div>
          <div className="mt-1 text-xs text-gray-500">{t("按天统计蜂窝数据上传与下载；从启用采样后开始累计")}</div>
        </div>
        <div className="flex gap-5 pr-1 text-xs">
          <div><span className="text-gray-500">{t("下载")}</span><span className="ml-2 font-mono font-semibold text-sky-500">{formatBytes(totals.rx)}</span></div>
          <div><span className="text-gray-500">{t("上传")}</span><span className="ml-2 font-mono font-semibold text-violet-500">{formatBytes(totals.tx)}</span></div>
        </div>
      </div>
      {loading ? (
        <div className="flex h-64 items-center justify-center text-sm text-gray-400">{t("流量图表加载中...")}</div>
      ) : (
        <EChart option={option} className="mt-2 h-64 w-full" />
      )}
      {error ? <div className="mt-1 text-xs text-red-500">{error}</div> : null}
    </div>
  );
}
