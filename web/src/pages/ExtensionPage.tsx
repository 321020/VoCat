import { useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { apiMessage } from "../api";
import { listPlugins, pluginAssetURL, type InstalledPlugin } from "../extensions";
import { ErrorState, ListSkeleton, PageHeader } from "../components/ui";
import { useI18n } from "../lib/i18n";

export default function ExtensionPage() {
  const { pluginId = "", contributionId = "" } = useParams();
  const { t, lang } = useI18n();
  const [plugins, setPlugins] = useState<InstalledPlugin[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    setLoading(true);
    listPlugins()
      .then((items) => setPlugins(items || []))
      .catch((reason) => setError(apiMessage(reason)))
      .finally(() => setLoading(false));
  }, [pluginId, contributionId]);

  const selected = useMemo(() => {
    const plugin = plugins.find((item) => item.id === pluginId && item.enabled);
    const contribution = plugin?.contributions.find((item) => item.id === contributionId && item.location === "sidebar");
    return plugin && contribution ? { plugin, contribution } : null;
  }, [plugins, pluginId, contributionId]);

  if (loading) return <ListSkeleton rows={6} />;
  if (error) return <ErrorState title={t("插件加载失败")} message={error} />;
  if (!selected) return <ErrorState title={t("插件不可用")} message={t("插件可能已被禁用、卸载或没有注册此页面。")} />;
  const label = lang === "zh" && selected.contribution.labelZh
    ? selected.contribution.labelZh
    : selected.contribution.label;
  return (
    <div className="mx-auto max-w-7xl">
      <PageHeader title={label} subtitle={`${selected.plugin.name} · ${selected.plugin.version}`} />
      <iframe
        title={label}
        src={pluginAssetURL(selected.plugin, selected.contribution)}
        className="h-[calc(100vh-10rem)] min-h-[560px] w-full rounded-xl border border-gray-200 bg-white dark:border-white/10 dark:bg-[#15151a]"
        sandbox="allow-scripts allow-forms allow-same-origin"
      />
    </div>
  );
}
