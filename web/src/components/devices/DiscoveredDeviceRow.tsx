import { cx } from "../../lib/utils";
import { Tag } from "../ui";
import type { DiscoveredDevice } from "../../types";
import { useI18n } from "../../lib/i18n";

export function DiscoveredDeviceRow({
  device,
  selected,
  modeLabel,
  isQmi,
  onSelect,
}: {
  device: DiscoveredDevice;
  selected: boolean;
  modeLabel: string;
  isQmi: boolean;
  onSelect: (d: DiscoveredDevice) => void;
}) {
  const { t } = useI18n();
  const degraded = !!device.degraded;
  return (
    <button
      type="button"
      aria-disabled={degraded}
      onClick={() => onSelect(device)}
      className={cx(
        "w-full rounded-xl border p-3 text-left",
        degraded && "cursor-not-allowed border-amber-200 bg-amber-50 opacity-85",
        !degraded && selected && "border-indigo-300 bg-indigo-50",
        !degraded && !selected && "border-gray-200 hover:bg-gray-50",
      )}
    >
      <div className="flex items-center gap-2 font-bold text-gray-800">
        <span>
          {device.netInterface || "--"} · {device.driverName || "--"}
        </span>
        <Tag type={isQmi ? "success" : "warning"}>{modeLabel}</Tag>
      </div>
      <div className="mt-0.5 truncate text-xs text-gray-500">
        {device.controlPath} · AT: {device.atPort || "--"} · IMEI: {device.imei || "--"} · USB: {device.usbPath || "--"}
      </div>
      {degraded ? <div className="mt-1 text-xs text-amber-700">{t("未找到可用的 AT 端口（串口可能仍在枚举），系统会自动重试；也可点击重新扫描。")}</div> : null}
    </button>
  );
}
