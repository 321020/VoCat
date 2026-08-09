// Tiny classnames helper (conditionally join class names).
export type ClassValue = string | number | null | false | undefined | Record<string, boolean>;

export function cx(...values: ClassValue[]): string {
  const out: string[] = [];
  for (const value of values) {
    if (!value) continue;
    if (typeof value === "string" || typeof value === "number") {
      out.push(String(value));
    } else if (typeof value === "object") {
      for (const key of Object.keys(value)) if (value[key]) out.push(key);
    }
  }
  return out.join(" ");
}

export function clamp(value: number, min: number, max: number) {
  return Math.min(max, Math.max(min, value));
}

// Signal strength (RSSI dBm, AT+CSQ) -> 0..4 bars, matching VoHive thresholds.
// RSSI-calibrated (not RSRP): RSSI sits ~20 dB above RSRP on LTE, so RSRP-scaled
// bands peg at full for any real signal and the bars never reflect strength.
export function signalBars(dbm?: number | null): number {
  if (typeof dbm !== "number" || !Number.isFinite(dbm) || dbm === 0 || dbm === -999) return 0;
  return dbm >= -70 ? 4 : dbm >= -85 ? 3 : dbm >= -100 ? 2 : 1;
}

export function signalValid(dbm?: number | null): boolean {
  return typeof dbm === "number" && Number.isFinite(dbm) && dbm !== 0 && dbm !== -999;
}

export function signalColor(dbm?: number | null): string {
  if (!signalValid(dbm)) return "bg-gray-300 dark:bg-gray-600";
  return dbm! >= -85 ? "bg-green-500" : dbm! >= -100 ? "bg-yellow-500" : "bg-red-500";
}

export function formatBytes(value?: number | null): string {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  return `${size >= 100 ? Math.round(size) : size.toFixed(1)} ${units[unit]}`;
}

export function formatTime(value?: string | number | Date | null): string {
  if (value === null || value === undefined || value === "") return "--";
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return "--";
  return date.toLocaleTimeString();
}

export function formatDateTime(value?: string | number | Date | null): string {
  if (value === null || value === undefined || value === "") return "--";
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return "--";
  return date.toLocaleString();
}

export function toTimestamp(value?: string | number | Date | null): number {
  if (value === null || value === undefined || value === "") return 0;
  const time = new Date(value as any).getTime();
  return Number.isFinite(time) ? time : 0;
}

// EC20-family Qualcomm module detection (matches backend parseATI: EC20/EC25).
export function isEC20Model(model?: string | null): boolean {
  if (!model) return false;
  const upper = model.toUpperCase();
  return upper.startsWith("EC20") || upper.startsWith("EC25");
}
