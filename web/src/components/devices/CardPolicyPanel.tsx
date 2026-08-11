import { CardUiRegular } from "@fluentui/react-icons";
import { Tag } from "../ui";
import { PolicySwitchCard } from "./PolicySwitchCard";
import { CardPolicyAPN } from "./CardPolicyAPN";
import { useCardPolicyToggles } from "./useCardPolicyToggles";
import { enableVoWiFi, disableVoWiFi, setFlightMode } from "./deviceActions";
import type { CardPolicy } from "../../types";
import { useI18n } from "../../lib/i18n";

export interface CardPolicyPanelProps {
  deviceId: string;
  iccid?: string;
  policy: CardPolicy | null;
  deviceOnline: boolean;
  onPolicyChanged: () => void;
}

export function CardPolicyPanel({ deviceId, iccid, policy, deviceOnline, onPolicyChanged }: CardPolicyPanelProps) {
  const { t } = useI18n();
  const operable = deviceOnline && !!iccid;
  const flags = policy
    ? { vowifiEnabled: policy.vowifiEnabled, airplaneEnabled: policy.airplaneEnabled }
    : null;

  const toggles = useCardPolicyToggles(flags, {
    applyVoWiFi: (value) => (deviceId ? (value ? enableVoWiFi(deviceId) : disableVoWiFi(deviceId)) : Promise.resolve({ ok: false })),
    applyAirplane: (value) => (deviceId ? setFlightMode(deviceId, value) : Promise.resolve({ ok: false })),
    onChanged: onPolicyChanged,
  });

  const isManual = policy?.source === "user" || policy?.source === "manual";
  const sourceLabel = policy ? (isManual ? t("手动设置") : t("自动默认")) : "";
  const { local } = toggles;

  return (
    <div>
      <div className="mb-4 flex items-center gap-3">
        <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-indigo-50 text-indigo-600 dark:bg-indigo-500/10 dark:text-indigo-400">
          <CardUiRegular className="text-[22px]" />
        </div>
        <div>
          <div className="text-lg font-bold text-gray-900 dark:text-white">{t("卡策略")}</div>
          <div className="text-xs text-gray-500 dark:text-gray-400">{t("VoWiFi / 飞行模式 开关跟着 SIM 卡走，切换即时生效")}</div>
        </div>
      </div>
      {!iccid ? (
        <div className="ui-panel-muted p-4 text-center text-sm text-gray-500 dark:text-gray-400">{t("设备尚未识别到 SIM 卡 ICCID，策略不可操作")}</div>
      ) : null}
      {iccid && !deviceOnline ? (
        <div className="mb-3 rounded-lg bg-yellow-50 px-3 py-2 text-xs text-yellow-700 dark:bg-yellow-900/20 dark:text-yellow-300">
          {t("设备离线，策略仅展示，切换操作已禁用")}
        </div>
      ) : null}
      {iccid ? (
        <div className="space-y-3">
          <div className="ui-panel-muted flex items-center justify-between p-3">
            <div>
              <div className="mb-0.5 text-xs font-bold uppercase tracking-wider text-gray-500">{t("当前卡 ICCID")}</div>
              <div className="font-mono text-sm text-gray-800 dark:text-gray-100">{iccid}</div>
            </div>
            {sourceLabel ? <Tag type={isManual ? "primary" : "info"}>{sourceLabel}</Tag> : null}
          </div>
          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            <PolicySwitchCard
              title="VoWiFi"
              subtitle={t("启用时强制关闭蜂窝射频；关闭 VoWiFi 后仍保持飞行模式")}
              tone="orange"
              checked={local.vowifiEnabled}
              disabled={!operable || toggles.vowifiPending}
              pending={toggles.vowifiPending}
              failed={toggles.vowifiFailed}
              onToggle={toggles.onVoWiFiToggle}
            />
            <PolicySwitchCard
              title={t("飞行模式")}
              subtitle={t("只有手动关闭此开关才允许设备连接基站")}
              tone="indigo"
              checked={local.airplaneEnabled}
              disabled={!operable || local.vowifiEnabled || toggles.airplanePending}
              pending={toggles.airplanePending}
              failed={toggles.airplaneFailed}
              onToggle={toggles.onAirplaneToggle}
            />
          </div>
          <CardPolicyAPN
            deviceId={deviceId}
            iccid={iccid}
            policy={policy}
            deviceOnline={deviceOnline}
            onSaved={onPolicyChanged}
          />
        </div>
      ) : null}
    </div>
  );
}
